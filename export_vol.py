"""Proxy LINEAR do previsor de vol (RV 30s forward) com features HAR que a engine
calcula ao vivo (rv 5s/30s/60s/5min + spread). Exporta vol.json. Go prevê log-RV
e converte σ=exp(pred)-eps."""
import json, numpy as np, pandas as pd
from sklearn.linear_model import Ridge
from mvp_perp_mm import build_day, TRAIN_DAYS, TEST_DAY

EPS = 0.01
FEATS = ["rv_5s", "rv_30s", "rv_60s", "rv_5m", "spread_bps"]

def feats(day):
    df = build_day(day).copy()
    r = np.log(df["mid"]).diff().fillna(0); r2 = r ** 2
    for w, lbl in [(50, "rv_5s"), (300, "rv_30s"), (600, "rv_60s"), (3000, "rv_5m")]:
        df[lbl] = np.sqrt(r2.rolling(w, 2).sum()) * 1e4
    df["y"] = np.log(np.sqrt(r2.rolling(300).sum().shift(-300)) * 1e4 + EPS)
    return df.dropna(subset=["y"]).replace([np.inf, -np.inf], 0).fillna(0)

tr = pd.concat([feats(d) for d in TRAIN_DAYS], ignore_index=True); te = feats(TEST_DAY)
mean = tr[FEATS].mean().values; std = tr[FEATS].std().replace(0, 1).values
m = Ridge(alpha=10.0).fit((tr[FEATS].values - mean) / std, tr["y"].values)
p = m.predict((te[FEATS].values - mean) / std); y = te["y"].values
r2 = 1 - np.sum((y - p) ** 2) / np.sum((y - y.mean()) ** 2)
print(f"proxy vol R²(log)={r2:.4f}  (CatBoost HAR era ~0.14)  corr={np.corrcoef(p,y)[0,1]:.4f}")
print("coefs:", dict(zip(FEATS, np.round(m.coef_, 4))))
json.dump({"feats": FEATS, "mean": mean.tolist(), "std": std.tolist(),
           "coef": m.coef_.tolist(), "intercept": float(m.intercept_)},
          open("vol.json", "w"), indent=2)
print("-> vol.json")
