"""Checagem cética do MVP: IC sobreposta vs NÃO-sobreposta + realidade de custo.
Amostras de 100ms adjacentes compartilham 9/10 da janela do alvo -> IC infla.
E o sinal (bps) tem que vencer o fee do perp (taker 5bps) ou ser feito como maker."""
from __future__ import annotations
import numpy as np, pandas as pd
from catboost import CatBoostRegressor
from mvp_perp_mm import build_day, add_features_target, FEATURES, TEST_DAY, H_BINS

te = add_features_target(build_day(TEST_DAY))
m = CatBoostRegressor().load_model("models/mvp_point.cbm")
pred = m.predict(te[FEATURES]); y = te["y"].values

def ic(p, a): return np.corrcoef(p, a)[0, 1]

print(f"=== IC sobreposta (todas {len(y):,} amostras): {ic(pred,y):.4f}")
# NÃO-sobreposta: 1 amostra a cada H bins (janelas de alvo independentes)
for s in (H_BINS, H_BINS*5):
    idx = np.arange(0, len(y), s)
    p2, a2 = pred[idx], y[idx]
    mask = a2 != 0
    da = (np.sign(p2[mask]) == np.sign(a2[mask])).mean()
    print(f"=== IC stride {s} ({len(idx):,} indep): IC={ic(p2,a2):.4f}  dir={da*100:.1f}%")

# realidade de custo: sinal (bps) vs fee do perp Deribit (taker 0.05% = 5 bps; maker 0%)
qe = pd.qcut(pd.Series(pred), 5, labels=False, duplicates="drop")
top = y[qe == qe.max()]; bot = y[qe == qe.min()]
edge = (top.mean() - bot.mean()) / 2  # edge médio por lado (long top / short bottom)
print(f"\n=== ECONOMIA ===")
print(f"  edge bruto top vs bottom: {edge:.3f} bps/lado")
print(f"  fee TAKER Deribit perp: 5.0 bps  -> tomar o sinal: {edge-5.0:.3f} bps (líquido)")
print(f"  fee MAKER: 0 bps (+rebate 1bp) -> só dá como MAKER, e aí entra seleção adversa")
print(f"  half-spread atual ~0.04 bps; tick=0.083 bps")
