package main

import (
	"encoding/json"
	"math"
	"sort"
	"time"
)

// F64 serializa NaN/Inf como null (encoding/json padrão quebra com NaN).
type F64 float64

func (f F64) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return []byte("null"), nil
	}
	return json.Marshal(v)
}

type Metrics struct {
	Name      string    `json:"name"`
	Bid       F64       `json:"bid"`
	Ask       F64       `json:"ask"`
	Mid       F64       `json:"mid"`
	Micro     F64       `json:"micro"`
	SpreadBps F64       `json:"spread_bps"`
	Imbalance F64       `json:"imbalance"`
	OFI       F64       `json:"ofi"`
	BidSz     F64       `json:"bid_sz"`
	AskSz     F64       `json:"ask_sz"`
	Kappa     F64       `json:"kappa"`      // decaimento da intensidade de fill (por bp)
	AParam    F64       `json:"a_param"`    // intensidade no mid
	TradesMin F64       `json:"trades_min"` // trades/min na janela
	RVAnn     F64       `json:"rv_ann"`     // vol realizada anualizada do mid
	NTrades   int       `json:"n_trades"`
	AgeMs     int64     `json:"age_ms"`
	MidHist   []float64 `json:"mid_hist"`
	SprdHist  []float64 `json:"sprd_hist"`
	// shadow-MM plain (BBO) — fills simulados no fluxo real, $0
	SMInv   F64 `json:"sm_inv"`
	SMPnL   F64 `json:"sm_pnl"`
	SMBuy   int `json:"sm_buy"`
	SMSell  int `json:"sm_sell"`
	SMRound int `json:"sm_round"`
	// shadow-MM com skew de inventário
	SKInv   F64 `json:"sk_inv"`
	SKPnL   F64 `json:"sk_pnl"`
	SKBuy   int `json:"sk_buy"`
	SKSell  int `json:"sk_sell"`
	SKRound int `json:"sk_round"`
	// maker GATEADO pelo sinal (markout +5s, net rebate, 30% haircut)
	SigScore F64 `json:"sig_score"`
	SigFills int `json:"sig_fills"`
	SigInv   F64 `json:"sig_inv"`
	SigMkSum F64 `json:"sig_mk_sum"` // markout total acumulado (bps)
	SigMkAvg F64 `json:"sig_mk_avg"` // markout médio por fill (bps)
	SigPosted int       `json:"sig_posted"`
	SigPend   int       `json:"sig_pend"`
	SigPnlUSD F64       `json:"sig_pnl_usd"` // PnL $ sobre banca 1000
	SigEquity []float64 `json:"sig_equity"`  // evolução da carteira
	SigVol    F64       `json:"sig_vol"`     // previsão de vol σ (bps, RV 30s)
	SigVolW   F64       `json:"sig_vol_w"`   // widen aplicado ao spread (σ/baseline)
}

func (m *Manager) Snapshot() []Metrics {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UnixMilli()
	out := make([]Metrics, 0, len(m.order))
	for _, name := range m.order {
		in := m.insts[name]
		met := Metrics{Name: name, NTrades: len(in.trades)}
		if in.bbP > 0 && in.baP > 0 {
			bp, bq := in.bbP, in.bbS
			ap, aq := in.baP, in.baS
			mid := (bp + ap) / 2
			met.Bid, met.Ask, met.Mid = F64(bp), F64(ap), F64(mid)
			met.BidSz, met.AskSz = F64(bq), F64(aq)
			if bq+aq > 0 {
				met.Micro = F64((ap*bq + bp*aq) / (bq + aq)) // micro-price (pesa por tamanho oposto)
				met.Imbalance = F64(bq / (bq + aq))
			}
			if mid > 0 {
				met.SpreadBps = F64((ap - bp) / mid * 1e4)
			}
			met.AgeMs = now - in.lastTS
			if met.AgeMs < 0 {
				met.AgeMs = 0 // relógio local pode estar atrás do timestamp da exchange
			}
			met.SMInv, met.SMPnL = F64(in.shPlain.inv), F64(in.shPlain.pnl(mid))
			met.SMBuy, met.SMSell, met.SMRound = in.shPlain.buy, in.shPlain.sell, in.shPlain.round()
			met.SKInv, met.SKPnL = F64(in.shSkew.inv), F64(in.shSkew.pnl(mid))
			met.SKBuy, met.SKSell, met.SKRound = in.shSkew.buy, in.shSkew.sell, in.shSkew.round()
			ofiSum := 0.0
			for _, o := range in.ofi {
				ofiSum += o.val
			}
			met.OFI = F64(ofiSum) // order-flow imbalance acumulado na janela (5s); + = pressão de compra
			if in.sig != nil {
				met.SigScore = F64(in.sig.score)
				met.SigFills, met.SigInv = in.sig.fills, F64(in.sig.inv)
				met.SigMkSum = F64(in.sig.markoutSum)
				if in.sig.fills > 0 {
					met.SigMkAvg = F64(in.sig.markoutSum / float64(in.sig.fills))
				}
				met.SigPosted = in.sig.posted
				met.SigPend = len(in.sig.pending)
				met.SigPnlUSD = F64(in.sig.pnlUSD)
				eq := in.sig.equity // downsample p/ sparkline (<=150 pts)
				step := 1
				if len(eq) > 150 {
					step = len(eq) / 150
				}
				for i := 0; i < len(eq); i += step {
					met.SigEquity = append(met.SigEquity, eq[i].val)
				}
				met.SigVol = F64(in.sigVol)
				if in.sigVolBase > 0 {
					w := in.sigVol / in.sigVolBase
					if w < 0.5 {
						w = 0.5
					} else if w > 3 {
						w = 3
					}
					met.SigVolW = F64(w)
				}
			}
		}
		// kappa (A-S) a partir dos trades — exige >=30s de janela p/ taxa estável
		if len(in.trades) >= 2 {
			winSec := float64(in.trades[len(in.trades)-1].ts-in.trades[0].ts) / 1000.0
			if winSec >= 30 {
				k, a, n := fitKappa(in.trades, winSec)
				met.Kappa, met.AParam = F64(k), F64(a)
				met.TradesMin = F64(float64(n) / winSec * 60.0)
			}
		}
		met.RVAnn = F64(realizedVol(in.mids))
		met.MidHist, met.SprdHist = downsample(in.mids, 150)
		out = append(out, met)
	}
	return out
}

// fitKappa: lambda(d) = A*exp(-kappa*d). Bina a distância (bps) dos trades ao mid
// e ajusta log(taxa) ~ d por OLS. slope = -kappa, intercept = log A.
func fitKappa(trades []trade, windowSec float64) (kappa, a float64, n int) {
	n = len(trades)
	if n < 20 || windowSec <= 0 {
		return math.NaN(), math.NaN(), n
	}
	ds := make([]float64, 0, n)
	for _, t := range trades {
		ds = append(ds, t.dBps)
	}
	sort.Float64s(ds)
	p95 := ds[int(0.95*float64(n))]
	if p95 <= 0 {
		return math.NaN(), math.NaN(), n
	}
	const B = 8
	bw := p95 / B
	bins := make([]int, B)
	for _, d := range ds {
		if d > p95 {
			continue
		}
		bi := int(d / bw)
		if bi >= B {
			bi = B - 1
		}
		bins[bi]++
	}
	var xs, ys []float64
	for i := 0; i < B; i++ {
		if bins[i] == 0 {
			continue
		}
		xs = append(xs, (float64(i)+0.5)*bw)
		ys = append(ys, math.Log(float64(bins[i])/windowSec))
	}
	if len(xs) < 2 {
		return math.NaN(), math.NaN(), n
	}
	slope, intercept := ols(xs, ys)
	return -slope, math.Exp(intercept), n
}

func ols(xs, ys []float64) (slope, intercept float64) {
	n := float64(len(xs))
	var sx, sy, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sy += ys[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, sy / n
	}
	slope = (n*sxy - sx*sy) / den
	intercept = (sy - slope*sx) / n
	return
}

// realizedVol anualizada a partir dos samples de mid (irregular -> normaliza por segundo).
func realizedVol(s []sample) float64 {
	if len(s) < 3 {
		return math.NaN()
	}
	var sumr2, totSec float64
	for i := 1; i < len(s); i++ {
		if s[i-1].mid <= 0 || s[i].mid <= 0 {
			continue
		}
		dt := float64(s[i].ts-s[i-1].ts) / 1000.0
		if dt <= 0 {
			continue
		}
		r := math.Log(s[i].mid / s[i-1].mid)
		sumr2 += r * r
		totSec += dt
	}
	if totSec <= 0 {
		return math.NaN()
	}
	return math.Sqrt(sumr2 / totSec * 365 * 24 * 3600)
}

func downsample(s []sample, n int) (mids, sprd []float64) {
	if len(s) == 0 {
		return
	}
	step := 1
	if len(s) > n {
		step = len(s) / n
	}
	for i := 0; i < len(s); i += step {
		mids = append(mids, s[i].mid)
		sprd = append(sprd, s[i].sprd)
	}
	return
}
