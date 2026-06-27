package main

// MM preliminar (pezinho na água) de opções no TESTNET.
// Cota os dois lados em torno do micro-price com skew de inventário, cancela/re-cota
// quando o fair se move (auto-cancel-on-move), rastreia fills/inventário/PnL.
// Cliente WS assíncrono (1 reader despacha respostas por id + subscriptions).
// Lê DERIBIT_CLIENT_ID/SECRET do env. cancela_all na entrada e na saída.

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type rpcResp struct {
	result json.RawMessage
	err    error
}

type DClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	idc     int
	pending map[int]chan rpcResp
	onSub   func(channel string, data json.RawMessage)
}

func dial(url string) (*DClient, error) {
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}
	d := &DClient{conn: c, pending: map[int]chan rpcResp{}}
	go d.readLoop()
	return d, nil
}

func (d *DClient) writeJSON(v any) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	return d.conn.WriteJSON(v)
}

func (d *DClient) readLoop() {
	for {
		_, data, err := d.conn.ReadMessage()
		if err != nil {
			d.mu.Lock()
			for _, ch := range d.pending {
				ch <- rpcResp{err: err}
			}
			d.pending = map[int]chan rpcResp{}
			d.mu.Unlock()
			return
		}
		var m struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
				Code    int    `json:"code"`
			} `json:"error"`
			Method string `json:"method"`
			Params struct {
				Type    string          `json:"type"`
				Channel string          `json:"channel"`
				Data    json.RawMessage `json:"data"`
			} `json:"params"`
		}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		switch {
		case m.Method == "heartbeat":
			if m.Params.Type == "test_request" {
				_ = d.writeJSON(map[string]any{"jsonrpc": "2.0", "id": 0, "method": "public/test", "params": map[string]any{}})
			}
		case m.Method == "subscription":
			if d.onSub != nil {
				d.onSub(m.Params.Channel, m.Params.Data)
			}
		default:
			d.mu.Lock()
			if ch, ok := d.pending[m.ID]; ok {
				delete(d.pending, m.ID)
				if m.Error != nil {
					ch <- rpcResp{err: fmt.Errorf("%s (code %d)", m.Error.Message, m.Error.Code)}
				} else {
					ch <- rpcResp{result: m.Result}
				}
			}
			d.mu.Unlock()
		}
	}
}

func (d *DClient) call(method string, params map[string]any) (json.RawMessage, error) {
	d.mu.Lock()
	d.idc++
	id := d.idc
	ch := make(chan rpcResp, 1)
	d.pending[id] = ch
	d.mu.Unlock()
	if err := d.writeJSON(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("timeout %s", method)
	}
}

// ---- estado do MM ----

type MM struct {
	mu          sync.Mutex
	inst        string
	tick        float64
	amount      float64
	halfFrac    float64 // half-spread como fração do fair
	skewFrac    float64 // deslocamento por unidade de inventário (fração do fair)
	bestBid     float64
	bestAsk     float64
	bidSz       float64
	askSz       float64
	fair        float64
	ourBidID    string
	ourAskID    string
	ourBidPx    float64
	ourAskPx    float64
	inventory   float64
	cash        float64 // fluxo de caixa dos fills (PnL = cash + inventory*fair)
	nFills      int
	nQuotes     int
	lastLog     string
}

func (m *MM) round(p float64) float64 { return math.Round(p/m.tick) * m.tick }

func (m *MM) snapshot() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	pnl := m.cash + m.inventory*m.fair
	return map[string]any{
		"instrument": m.inst, "fair": m.fair, "best_bid": m.bestBid, "best_ask": m.bestAsk,
		"our_bid": m.ourBidPx, "our_ask": m.ourAskPx, "inventory": m.inventory,
		"pnl": pnl, "n_fills": m.nFills, "n_quotes": m.nQuotes, "last": m.lastLog,
	}
}

func runMM(instrument string, durationSec int) {
	id := os.Getenv("DERIBIT_CLIENT_ID")
	sec := os.Getenv("DERIBIT_CLIENT_SECRET")
	if id == "" || sec == "" {
		log.Fatal("defina DERIBIT_CLIENT_ID e DERIBIT_CLIENT_SECRET no env")
	}
	d, err := dial(wsTest)
	if err != nil {
		log.Fatal("dial testnet: ", err)
	}
	if _, err := d.call("public/auth", map[string]any{
		"grant_type": "client_credentials", "client_id": id, "client_secret": sec}); err != nil {
		log.Fatal("auth: ", err)
	}
	log.Printf("✓ autenticado no testnet | MM em %s", instrument)

	isPerp := len(instrument) >= 9 && instrument[len(instrument)-9:] == "PERPETUAL"
	mm := &MM{inst: instrument, halfFrac: 0.02, skewFrac: 0.01}
	if isPerp {
		mm.tick, mm.amount = 0.5, 10
	} else {
		mm.tick, mm.amount = 0.0005, 0.1
	}

	d.onSub = func(channel string, data json.RawMessage) {
		switch {
		case len(channel) > 5 && channel[:5] == "book.":
			var b struct {
				Bids [][]interface{} `json:"bids"`
				Asks [][]interface{} `json:"asks"`
			}
			if json.Unmarshal(data, &b) != nil {
				return
			}
			bids, asks := parseLevels(b.Bids), parseLevels(b.Asks)
			mm.mu.Lock()
			if len(bids) > 0 {
				mm.bestBid, mm.bidSz = bids[0][0], bids[0][1]
			}
			if len(asks) > 0 {
				mm.bestAsk, mm.askSz = asks[0][0], asks[0][1]
			}
			if mm.bestBid > 0 && mm.bestAsk > 0 && mm.bidSz+mm.askSz > 0 {
				mm.fair = (mm.bestAsk*mm.bidSz + mm.bestBid*mm.askSz) / (mm.bidSz + mm.askSz)
			}
			mm.mu.Unlock()
		case len(channel) > 11 && channel[:11] == "user.trades":
			var ts []struct {
				Price     float64 `json:"price"`
				Amount    float64 `json:"amount"`
				Direction string  `json:"direction"`
			}
			if json.Unmarshal(data, &ts) != nil {
				return
			}
			mm.mu.Lock()
			for _, t := range ts {
				if t.Direction == "buy" {
					mm.inventory += t.Amount
					mm.cash -= t.Price * t.Amount
				} else {
					mm.inventory -= t.Amount
					mm.cash += t.Price * t.Amount
				}
				mm.nFills++
				log.Printf("FILL %s %.4f @ %.4f -> inv=%.4f", t.Direction, t.Amount, t.Price, mm.inventory)
			}
			mm.mu.Unlock()
		}
	}

	// subscribe book (público) + user.trades (privado, após auth)
	_, _ = d.call("public/subscribe", map[string]any{"channels": []string{
		fmt.Sprintf("book.%s.none.5.100ms", instrument)}})
	_, _ = d.call("private/subscribe", map[string]any{"channels": []string{
		fmt.Sprintf("user.trades.%s.raw", instrument)}})

	// fair inicial via ticker (caso book esteja vazio no testnet)
	if r, err := d.call("public/ticker", map[string]any{"instrument_name": instrument}); err == nil {
		var tk struct {
			MarkPrice float64 `json:"mark_price"`
		}
		_ = json.Unmarshal(r, &tk)
		mm.mu.Lock()
		if mm.fair == 0 {
			mm.fair = tk.MarkPrice
		}
		mm.mu.Unlock()
	}

	cancelAll := func() { _, _ = d.call("private/cancel_all_by_instrument", map[string]any{"instrument_name": instrument}) }
	cancelAll() // começa limpo

	// loop de quoting
	stop := make(chan struct{})
	if durationSec > 0 {
		go func() { time.Sleep(time.Duration(durationSec) * time.Second); close(stop) }()
	}
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	go mm.serve()
	log.Printf("loop de quoting iniciado (half=%.0f%% skew=%.0f%% size=%.3f tick=%g)",
		mm.halfFrac*100, mm.skewFrac*100, mm.amount, mm.tick)

	for {
		select {
		case <-stop:
			cancelAll()
			s := mm.snapshot()
			log.Printf("FIM | quotes=%v fills=%v inv=%v pnl=%v", s["n_quotes"], s["n_fills"], s["inventory"], s["pnl"])
			return
		case <-ticker.C:
			mm.requote(d)
		}
	}
}

func (m *MM) requote(d *DClient) {
	m.mu.Lock()
	fair := m.fair
	if fair <= 0 {
		m.mu.Unlock()
		return
	}
	half := fair * m.halfFrac
	skew := fair * m.skewFrac * m.inventory // inventário longo -> abaixa os 2 lados (vende mais fácil)
	wantBid := m.round(fair - half - skew)
	wantAsk := m.round(fair + half - skew)
	curBid, curAsk := m.ourBidPx, m.ourAskPx
	bidID, askID := m.ourBidID, m.ourAskID
	amt := m.amount
	inst := m.inst
	m.mu.Unlock()

	// re-cota só se o alvo mudou > 1 tick (auto-cancel-on-move)
	if math.Abs(wantBid-curBid) >= m.tick {
		if bidID != "" {
			_, _ = d.call("private/cancel", map[string]any{"order_id": bidID})
		}
		if r, err := d.call("private/buy", map[string]any{
			"instrument_name": inst, "amount": amt, "type": "limit", "price": wantBid,
			"post_only": true, "label": "mm"}); err == nil {
			m.setOrder(true, orderID(r), wantBid)
		}
	}
	if math.Abs(wantAsk-curAsk) >= m.tick {
		if askID != "" {
			_, _ = d.call("private/cancel", map[string]any{"order_id": askID})
		}
		if r, err := d.call("private/sell", map[string]any{
			"instrument_name": inst, "amount": amt, "type": "limit", "price": wantAsk,
			"post_only": true, "label": "mm"}); err == nil {
			m.setOrder(false, orderID(r), wantAsk)
		}
	}
	m.mu.Lock()
	m.nQuotes++
	m.lastLog = fmt.Sprintf("fair=%.4f bid=%.4f ask=%.4f inv=%.3f", fair, wantBid, wantAsk, m.inventory)
	m.mu.Unlock()
}

func (m *MM) setOrder(isBid bool, oid string, px float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if isBid {
		m.ourBidID, m.ourBidPx = oid, px
	} else {
		m.ourAskID, m.ourAskPx = oid, px
	}
}

func orderID(r json.RawMessage) string {
	var o struct {
		Order struct {
			OrderID string `json:"order_id"`
		} `json:"order"`
	}
	_ = json.Unmarshal(r, &o)
	return o.Order.OrderID
}

func (m *MM) serve() {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/mm", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.snapshot())
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(mmHTML))
	})
	log.Println("MM dashboard em http://localhost:8081")
	_ = http.ListenAndServe(":8081", mux)
}

const mmHTML = `<!doctype html><meta charset=utf-8><title>MM testnet</title>
<style>body{font:14px ui-monospace,monospace;background:#0d1117;color:#e6edf3;max-width:560px;margin:24px auto;padding:0 16px}
h1{font-size:16px}table{width:100%;border-collapse:collapse}td{padding:4px 8px;border-bottom:1px solid #26303d}
td:last-child{text-align:right;font-variant-numeric:tabular-nums}.g{color:#3fb950}.r{color:#f85149}</style>
<h1>MM preliminar — testnet <span style="color:#8b98a8">(pezinho na água)</span></h1>
<table id=t></table>
<script>
const F=(x,d=4)=>x==null?'—':Number(x).toLocaleString('en-US',{maximumFractionDigits:d});
async function tick(){let m;try{m=await(await fetch('/api/mm')).json()}catch(e){return}
 document.getElementById('t').innerHTML=
  '<tr><td>instrumento</td><td>'+m.instrument+'</td></tr>'+
  '<tr><td>fair (micro)</td><td>'+F(m.fair)+'</td></tr>'+
  '<tr><td>mercado bid / ask</td><td>'+F(m.best_bid)+' / '+F(m.best_ask)+'</td></tr>'+
  '<tr><td>NOSSO bid / ask</td><td class=g>'+F(m.our_bid)+' / '+F(m.our_ask)+'</td></tr>'+
  '<tr><td>inventário</td><td>'+F(m.inventory,3)+'</td></tr>'+
  '<tr><td>PnL (MTM)</td><td class="'+(m.pnl>=0?'g':'r')+'">'+F(m.pnl,4)+'</td></tr>'+
  '<tr><td>quotes / fills</td><td>'+m.n_quotes+' / '+m.n_fills+'</td></tr>'+
  '<tr><td>último</td><td>'+(m.last||'—')+'</td></tr>';}
tick();setInterval(tick,1000);
</script>`
