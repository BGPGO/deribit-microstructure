package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsURL   = "wss://www.deribit.com/ws/api/v2"
	restURL = "https://www.deribit.com/api/v2/public/"
	tradeWindowMs = 10 * 60 * 1000 // janela de trades p/ estimar kappa
	midCap        = 12000          // samples de mid (event-level)
	ofiWindowMs   = 5000           // janela do OFI (5s)
)

type ofiInc struct {
	ts  int64
	val float64
}

type trade struct {
	ts    int64
	price float64
	amt   float64
	dir   string
	dBps  float64 // distância do mid no momento do trade, em bps
}

type sample struct {
	ts   int64
	mid  float64
	sprd float64 // spread em bps
}

type Inst struct {
	Name   string
	bidMap map[float64]float64 // book COMPLETO: preço -> tamanho (via deltas incrementais)
	askMap map[float64]float64
	bbP, bbS, baP, baS float64  // best bid/ask preço,tamanho
	lastID             int64    // change_id p/ detectar gap
	pbP, pbS, paP, paS float64  // best anterior (p/ OFI)
	ofi                []ofiInc // incrementos de OFI (janela)
	lastTS             int64
	trades             []trade
	mids               []sample
	// dois shadow-MM no mesmo fluxo real (zero $): plain (BBO) vs com skew de inventário
	shPlain *ShadowBook
	shSkew  *ShadowBook
	sig     *SignalBook // maker GATEADO pelo sinal (markout +5s, rebate, 30% haircut)
	sigVol     float64  // previsão de vol (σ, bps) — proxy linear HAR, atualizado 1/s
	sigVolBase float64  // baseline EWMA do σ (p/ dimensionar o widen)
}

// volFeatures: rv 5s/30s/60s/5min (do buffer de mids) + spread. now = exchange ts (ms).
func (in *Inst) volFeatures(now int64) []float64 {
	rv := func(winMs int64) float64 {
		cut := now - winMs
		var sumr2, prev float64
		for k := 0; k < len(in.mids); k++ {
			if in.mids[k].ts < cut {
				continue
			}
			m := in.mids[k].mid
			if prev > 0 && m > 0 {
				r := math.Log(m / prev)
				sumr2 += r * r
			}
			prev = m
		}
		return math.Sqrt(sumr2) * 1e4
	}
	spread := 0.0
	if in.bbP > 0 && in.baP > 0 {
		spread = (in.baP - in.bbP) / ((in.bbP + in.baP) / 2) * 1e4
	}
	return []float64{rv(5000), rv(30000), rv(60000), rv(300000), spread}
}

// featureVec: as 6 features do proxy linear, na ordem do signal.json.
func (in *Inst) featureVec(mid float64) []float64 {
	bq, aq := in.bbS, in.baS
	micro := mid
	imbC := 0.0
	if bq+aq > 0 {
		micro = (in.baP*bq + in.bbP*aq) / (bq + aq)
		imbC = bq/(bq+aq) - 0.5
	}
	microGap := (micro - mid) / mid * 1e4
	spread := (in.baP - in.bbP) / mid * 1e4
	ofi5s := 0.0
	for _, o := range in.ofi { // já é janela 5s
		ofi5s += o.val
	}
	sv := 0.0
	cut := in.lastTS - 5000
	for k := len(in.trades) - 1; k >= 0 && in.trades[k].ts >= cut; k-- {
		if in.trades[k].dir == "buy" {
			sv += in.trades[k].amt
		} else {
			sv -= in.trades[k].amt
		}
	}
	ret1s, cut1 := 0.0, in.lastTS-1000
	for k := len(in.mids) - 1; k >= 0; k-- {
		if in.mids[k].ts <= cut1 && in.mids[k].mid > 0 {
			ret1s = math.Log(mid/in.mids[k].mid) * 1e4
			break
		}
	}
	return []float64{microGap, imbC, spread, ofi5s, sv, ret1s}
}

func (in *Inst) midNow() float64 {
	if in.bbP > 0 && in.baP > 0 {
		return (in.bbP + in.baP) / 2
	}
	return 0
}

func (in *Inst) recomputeBest() {
	in.bbP, in.bbS, in.baP, in.baS = 0, 0, 0, 0
	for p, s := range in.bidMap {
		if s > 0 && (in.bbP == 0 || p > in.bbP) {
			in.bbP, in.bbS = p, s
		}
	}
	for p, s := range in.askMap {
		if s > 0 && (in.baP == 0 || p < in.baP) {
			in.baP, in.baS = p, s
		}
	}
}

type FillEvent struct {
	Ts    int64   `json:"ts"`
	Inst  string  `json:"inst"`
	Book  string  `json:"book"`
	Side  string  `json:"side"`
	Price float64 `json:"price"`
}

type Manager struct {
	mu     sync.Mutex
	insts  map[string]*Inst
	order  []string
	depth  int
	sig    *SigModel // proxy linear de direção (signal.json)
	volM   *SigModel // proxy linear de vol (vol.json)
	events []FillEvent
}

// RunVolLoop: a cada 1s, atualiza σ (e baseline EWMA) de cada instrumento.
func (m *Manager) RunVolLoop() {
	if m.volM == nil {
		return
	}
	t := time.NewTicker(time.Second)
	for range t.C {
		m.mu.Lock()
		for _, in := range m.insts {
			if in.lastTS == 0 || len(in.mids) < 5 {
				continue
			}
			sigma := math.Exp(m.volM.score(in.volFeatures(in.lastTS))) - 0.01
			if sigma < 0 {
				sigma = 0
			}
			in.sigVol = sigma
			if in.sigVolBase == 0 {
				in.sigVolBase = sigma
			} else {
				in.sigVolBase = 0.98*in.sigVolBase + 0.02*sigma
			}
		}
		m.mu.Unlock()
	}
}

func (m *Manager) addEvent(e FillEvent) {
	m.events = append(m.events, e)
	if len(m.events) > 200 {
		m.events = m.events[len(m.events)-200:]
	}
}

func (m *Manager) Events() []FillEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.events)
	k := 40
	if n < k {
		k = n
	}
	out := make([]FillEvent, k)
	copy(out, m.events[n-k:])
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // newest first
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func NewManager(list []string, depth int, sig, volM *SigModel) *Manager {
	m := &Manager{insts: map[string]*Inst{}, depth: depth, sig: sig, volM: volM}
	for _, name := range list {
		m.insts[name] = &Inst{Name: name,
			bidMap: map[float64]float64{}, askMap: map[float64]float64{}, lastID: -1,
			shPlain: &ShadowBook{size: 1, skewFrac: 0},       // BBO puro
			shSkew:  &ShadowBook{size: 1, skewFrac: 0.00004}, // ~4bps por unidade de inventário (cap 30)
			sig:     &SignalBook{},
		}
		m.order = append(m.order, name)
	}
	return m
}

func (m *Manager) RunForever() {
	backoff := time.Second
	for {
		start := time.Now()
		err := m.runConn()
		if time.Since(start) > 10*time.Second {
			backoff = time.Second // conexão durou — reseta o backoff
		}
		log.Printf("ws caiu: %v — reconnect em %s", err, backoff)
		time.Sleep(backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (m *Manager) runConn() error {
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return c.WriteJSON(v)
	}
	// heartbeat (mantém a conexão viva)
	_ = writeJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "public/set_heartbeat",
		"params": map[string]any{"interval": 30}})
	// subscribe book + trades de cada instrumento
	var chans []string
	for _, name := range m.order {
		// incremental (deltas new/change/delete, book completo). 'raw' (por evento) exige auth;
		// '.100ms' é sem-auth, coalescido a 100ms. Pra raw de verdade ao vivo: auth com key de produção.
		chans = append(chans, fmt.Sprintf("book.%s.100ms", name))
		chans = append(chans, fmt.Sprintf("trades.%s.100ms", name))
	}
	if err := writeJSON(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "public/subscribe",
		"params": map[string]any{"channels": chans}}); err != nil {
		return err
	}
	log.Printf("conectado, subscrito a %d canais", len(chans))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			return err
		}
		m.handle(data, writeJSON)
	}
}

func (m *Manager) handle(data []byte, writeJSON func(any) error) {
	var p struct {
		Method string `json:"method"`
		Params struct {
			Type    string          `json:"type"`
			Channel string          `json:"channel"`
			Data    json.RawMessage `json:"data"`
		} `json:"params"`
	}
	if json.Unmarshal(data, &p) != nil {
		return
	}
	if p.Method == "" { // resposta de RPC (subscribe/erro) — loga p/ debug
		var e struct {
			Error  json.RawMessage `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error != nil {
			log.Printf("RPC erro: %s", e.Error)
		} else if e.Result != nil {
			log.Printf("RPC ok: %.120s", e.Result)
		}
	}
	switch p.Method {
	case "heartbeat":
		if p.Params.Type == "test_request" {
			_ = writeJSON(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "public/test", "params": map[string]any{}})
		}
	case "subscription":
		switch {
		case strings.HasPrefix(p.Params.Channel, "book."):
			m.onBook(p.Params.Channel, p.Params.Data, writeJSON)
		case strings.HasPrefix(p.Params.Channel, "trades."):
			m.onTrades(p.Params.Data)
		}
	}
}

// parseLevels: book agrupado [preço,tam] ou ["new",preço,tam] (usado pelo MM testnet em mm.go).
func parseLevels(raw [][]interface{}) [][2]float64 {
	out := make([][2]float64, 0, len(raw))
	for _, row := range raw {
		var nums []float64
		for _, v := range row {
			if f, ok := v.(float64); ok {
				nums = append(nums, f)
			}
		}
		if len(nums) >= 2 {
			out = append(out, [2]float64{nums[len(nums)-2], nums[len(nums)-1]})
		}
	}
	return out
}

// parseDelta lê uma linha do canal incremental: [action(string), price, amount].
func parseDelta(row []interface{}) (action string, price, amount float64) {
	var nums []float64
	for _, v := range row {
		switch x := v.(type) {
		case string:
			action = x
		case float64:
			nums = append(nums, x)
		}
	}
	if len(nums) >= 2 {
		price, amount = nums[0], nums[1]
	}
	return
}

func applyDeltas(m map[float64]float64, rows [][]interface{}) {
	for _, row := range rows {
		action, price, amount := parseDelta(row)
		if action == "delete" || amount == 0 {
			delete(m, price)
		} else {
			m[price] = amount
		}
	}
}

func (m *Manager) onBook(channel string, raw json.RawMessage, writeJSON func(any) error) {
	var b struct {
		Instrument   string          `json:"instrument_name"`
		Timestamp    int64           `json:"timestamp"`
		Type         string          `json:"type"`
		ChangeID     int64           `json:"change_id"`
		PrevChangeID int64           `json:"prev_change_id"`
		Bids         [][]interface{} `json:"bids"`
		Asks         [][]interface{} `json:"asks"`
	}
	if json.Unmarshal(raw, &b) != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	in := m.insts[b.Instrument]
	if in == nil {
		return
	}

	if b.Type == "snapshot" {
		in.bidMap = map[float64]float64{}
		in.askMap = map[float64]float64{}
		applyDeltas(in.bidMap, b.Bids)
		applyDeltas(in.askMap, b.Asks)
		in.lastID = b.ChangeID
		in.recomputeBest()
		in.pbP, in.pbS, in.paP, in.paS = in.bbP, in.bbS, in.baP, in.baS // sem OFI no snapshot
		in.lastTS = b.Timestamp
		return
	}
	// change: detecta gap de sequência -> resync (reassina p/ novo snapshot)
	if in.lastID >= 0 && b.PrevChangeID != in.lastID {
		log.Printf("gap em %s (prev=%d last=%d) -> resync", b.Instrument, b.PrevChangeID, in.lastID)
		_ = writeJSON(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "public/unsubscribe", "params": map[string]any{"channels": []string{channel}}})
		_ = writeJSON(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "public/subscribe", "params": map[string]any{"channels": []string{channel}}})
		in.lastID = -1
		return
	}
	applyDeltas(in.bidMap, b.Bids)
	applyDeltas(in.askMap, b.Asks)
	in.lastID = b.ChangeID
	in.recomputeBest()
	in.lastTS = b.Timestamp
	if in.bbP <= 0 || in.baP <= 0 {
		return
	}

	// OFI (Cont-Kukanov-Stoikov, best level): +pressão de compra, -pressão de venda
	eb := 0.0
	switch {
	case in.bbP > in.pbP:
		eb = in.bbS
	case in.bbP == in.pbP:
		eb = in.bbS - in.pbS
	default:
		eb = -in.pbS
	}
	ea := 0.0
	switch {
	case in.baP < in.paP:
		ea = in.baS
	case in.baP == in.paP:
		ea = in.baS - in.paS
	default:
		ea = -in.paS
	}
	in.ofi = append(in.ofi, ofiInc{b.Timestamp, eb - ea})
	cut := b.Timestamp - ofiWindowMs
	i := 0
	for i < len(in.ofi) && in.ofi[i].ts < cut {
		i++
	}
	in.ofi = in.ofi[i:]
	in.pbP, in.pbS, in.paP, in.paS = in.bbP, in.bbS, in.baP, in.baS

	mid := (in.bbP + in.baP) / 2
	in.shPlain.quote(in.bbP, in.baP, mid)
	in.shSkew.quote(in.bbP, in.baP, mid)
	if m.sig != nil { // maker gateado pelo sinal, spread dimensionado pela vol (A-S)
		spreadBps := (in.baP - in.bbP) / mid * 1e4
		widen := 1.0
		if in.sigVolBase > 0 && in.sigVol > 0 {
			widen = in.sigVol / in.sigVolBase
			if widen < 0.5 {
				widen = 0.5
			} else if widen > 3 {
				widen = 3
			}
		}
		in.sig.quote(m.sig.score(in.featureVec(mid)), mid, spreadBps/2*widen)
		in.sig.resolve(b.Timestamp, mid)
	}
	sprd := (in.baP - in.bbP) / mid * 1e4
	in.mids = append(in.mids, sample{b.Timestamp, mid, sprd})
	if len(in.mids) > midCap {
		in.mids = in.mids[len(in.mids)-midCap:]
	}
}

func (m *Manager) onTrades(raw json.RawMessage) {
	var ts []struct {
		Instrument string  `json:"instrument_name"`
		Price      float64 `json:"price"`
		Amount     float64 `json:"amount"`
		Direction  string  `json:"direction"`
		Timestamp  int64   `json:"timestamp"`
	}
	if json.Unmarshal(raw, &ts) != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range ts {
		in := m.insts[t.Instrument]
		if in == nil {
			continue
		}
		mid := in.midNow()
		dbps := 0.0
		if mid > 0 {
			dbps = math.Abs(t.Price-mid) / mid * 1e4
		}
		in.trades = append(in.trades, trade{t.Timestamp, t.Price, t.Amount, t.Direction, dbps})
		if bf, af := in.shPlain.onTrade(t.Price, t.Direction); bf || af { // fills simulados + feed
			if bf {
				m.addEvent(FillEvent{t.Timestamp, t.Instrument, "plain", "BUY", in.shPlain.bid})
			}
			if af {
				m.addEvent(FillEvent{t.Timestamp, t.Instrument, "plain", "SELL", in.shPlain.ask})
			}
		}
		in.shSkew.onTrade(t.Price, t.Direction)
		if ok, side, px := in.sig.onTrade(t.Price, t.Direction, t.Timestamp); ok {
			m.addEvent(FillEvent{t.Timestamp, t.Instrument, "SINAL", side, px})
		}
		cut := t.Timestamp - tradeWindowMs
		i := 0
		for i < len(in.trades) && in.trades[i].ts < cut {
			i++
		}
		in.trades = in.trades[i:]
	}
}

// ---- REST: resolve a opção ATM ~targetDays dias (retorna o nome da CALL) ----

func restGet(method string, params map[string]string) (json.RawMessage, error) {
	q := []string{}
	for k, v := range params {
		q = append(q, k+"="+v)
	}
	resp, err := http.Get(restURL + method + "?" + strings.Join(q, "&"))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	return r.Result, nil
}

func resolveATMOption(ccy string, targetDays float64) (string, error) {
	res, err := restGet("get_instruments", map[string]string{"currency": ccy, "kind": "option", "expired": "false"})
	if err != nil {
		return "", err
	}
	var insts []struct {
		Name   string  `json:"instrument_name"`
		Expiry int64   `json:"expiration_timestamp"`
		Strike float64 `json:"strike"`
	}
	if err := json.Unmarshal(res, &insts); err != nil {
		return "", err
	}
	if len(insts) == 0 {
		return "", fmt.Errorf("sem opções")
	}
	// índice spot
	idxRes, err := restGet("get_index_price", map[string]string{"index_name": strings.ToLower(ccy) + "_usd"})
	if err != nil {
		return "", err
	}
	var idx struct {
		IndexPrice float64 `json:"index_price"`
	}
	_ = json.Unmarshal(idxRes, &idx)
	// expiry mais próximo de targetDays
	var exps []int64
	seen := map[int64]bool{}
	for _, i := range insts {
		if !seen[i.Expiry] {
			seen[i.Expiry] = true
			exps = append(exps, i.Expiry)
		}
	}
	sort.Slice(exps, func(a, b int) bool { return exps[a] < exps[b] })
	now := time.Now().UnixMilli()
	bestExp := exps[0]
	bestDiff := math.Inf(1)
	for _, e := range exps {
		d := math.Abs(float64(e-now)/86400000.0 - targetDays)
		if d < bestDiff {
			bestDiff = d
			bestExp = e
		}
	}
	// strike mais próximo do índice, CALL
	best := ""
	bestK := math.Inf(1)
	for _, i := range insts {
		if i.Expiry != bestExp || !strings.HasSuffix(i.Name, "-C") {
			continue
		}
		if d := math.Abs(i.Strike - idx.IndexPrice); d < bestK {
			bestK = d
			best = i.Name
		}
	}
	if best == "" {
		return "", fmt.Errorf("sem call no expiry alvo")
	}
	return best, nil
}
