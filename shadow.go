package main

import "math"

// ShadowBook: um market maker SHADOW (fills simulados no fluxo real, zero $).
// Cota no BBO, deslocado por skew de inventário. Otimista (assume front-of-queue):
// preenche em todo trade que cruza nosso preço.
//
//	skewFrac = 0      -> plain (cota exatamente no BBO)
//	skewFrac > 0      -> desloca o anchor por skewFrac*mid*inv (long -> abaixa os 2 lados)
type ShadowBook struct {
	bid, ask  float64
	inv, cash float64
	buy, sell int
	size      float64
	skewFrac  float64
}

const invCap = 30.0 // limita o efeito do skew p/ não explodir

func (s *ShadowBook) quote(bestBid, bestAsk, mid float64) {
	shift := 0.0
	if s.skewFrac != 0 && mid > 0 {
		inv := math.Max(-invCap, math.Min(invCap, s.inv))
		shift = mid * s.skewFrac * inv // inv>0 (long) -> shift>0 -> abaixa bid e ask (vende mais fácil)
	}
	s.bid = bestBid - shift
	s.ask = bestAsk - shift
}

func (s *ShadowBook) onTrade(price float64, dir string) (bidFill, askFill bool) {
	if dir == "sell" && s.bid > 0 && price <= s.bid {
		s.inv += s.size
		s.cash -= s.bid * s.size
		s.buy++
		bidFill = true
	}
	if dir == "buy" && s.ask > 0 && price >= s.ask {
		s.inv -= s.size
		s.cash += s.ask * s.size
		s.sell++
		askFill = true
	}
	return
}

func (s *ShadowBook) pnl(mid float64) float64 { return s.cash + s.inv*mid }

func (s *ShadowBook) round() int {
	if s.buy < s.sell {
		return s.buy
	}
	return s.sell
}
