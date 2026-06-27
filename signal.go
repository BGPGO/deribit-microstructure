package main

import (
	"encoding/json"
	"math/rand"
	"os"
)

const (
	markoutMs = 5000   // horizonte de markout: 5s
	fillKeep  = 0.70   // 30% de depreciação: simula queue miss (só 70% dos fills "pegam")
	rebateBps = 1.0    // rebate maker Deribit perp (0.01%)
	sigSize   = 1.0
	bankroll  = 1000.0 // banca assumida; cada fill negocia o notional cheio (1x)
)

type eqPoint struct {
	ts  int64
	val float64
}

type SigModel struct {
	Feats     []string  `json:"feats"`
	Mean      []float64 `json:"mean"`
	Std       []float64 `json:"std"`
	Coef      []float64 `json:"coef"`
	Intercept float64   `json:"intercept"`
}

func loadSigModel(path string) *SigModel {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m SigModel
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return &m
}

// score linear: x em ordem [micro_gap, imb_c, spread_bps, ofi_5s, sv_5s, ret_1s]
func (m *SigModel) score(x []float64) float64 {
	s := m.Intercept
	for i := range m.Coef {
		if i < len(x) && m.Std[i] != 0 {
			s += m.Coef[i] * (x[i] - m.Mean[i]) / m.Std[i]
		}
	}
	return s
}

type pendFill struct {
	ts    int64
	side  float64 // +1 long (bid), -1 short (ask)
	entry float64
}

// SignalBook: maker GATEADO pelo sinal. Só posta o lado previsto. Mede markout
// condicionado a fill (+5s), net do rebate, com 30% de depreciação nos fills.
type SignalBook struct {
	bid, ask   float64
	side       int     // +1 posta só bid; -1 posta só ask; 0 nenhum
	score      float64
	pending    []pendFill
	fills      int
	markoutSum float64 // bps acumulado (net rebate)
	pnlUSD     float64 // PnL em $ sobre a banca (mk/1e4 * bankroll por fill)
	equity     []eqPoint
	inv        float64
	posted     int
}

// quote: posta em mid ± deltaBps (A-S: deltaBps dimensionado pela vol).
func (sb *SignalBook) quote(score, mid, deltaBps float64) {
	sb.score = score
	d := mid * deltaBps / 1e4
	sb.bid, sb.ask = mid-d, mid+d
	if score > 0 {
		sb.side = 1
	} else if score < 0 {
		sb.side = -1
	} else {
		sb.side = 0
	}
	if sb.side != 0 {
		sb.posted++
	}
}

func (sb *SignalBook) onTrade(price float64, dir string, ts int64) (filled bool, side string, entry float64) {
	// posta bid (long) -> SELL cruzando preenche; posta ask (short) -> BUY cruzando preenche
	if sb.side == 1 && dir == "sell" && sb.bid > 0 && price <= sb.bid && rand.Float64() < fillKeep {
		sb.pending = append(sb.pending, pendFill{ts, 1, sb.bid})
		return true, "LONG", sb.bid
	}
	if sb.side == -1 && dir == "buy" && sb.ask > 0 && price >= sb.ask && rand.Float64() < fillKeep {
		sb.pending = append(sb.pending, pendFill{ts, -1, sb.ask})
		return true, "SHORT", sb.ask
	}
	return false, "", 0
}

func (sb *SignalBook) resolve(nowTs int64, mid float64) {
	keep := sb.pending[:0]
	for _, f := range sb.pending {
		if nowTs-f.ts >= markoutMs && f.entry > 0 {
			mk := f.side*(mid-f.entry)/f.entry*1e4 + rebateBps
			sb.markoutSum += mk
			sb.pnlUSD += mk / 1e4 * bankroll // notional 1x = banca
			sb.equity = append(sb.equity, eqPoint{nowTs, bankroll + sb.pnlUSD})
			if len(sb.equity) > 1000 {
				sb.equity = sb.equity[len(sb.equity)-1000:]
			}
			sb.fills++
			sb.inv += f.side * sigSize
		} else {
			keep = append(keep, f)
		}
	}
	sb.pending = keep
}
