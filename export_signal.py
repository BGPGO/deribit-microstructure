"""Fita um proxy LINEAR da direção (1s) usando só features que a engine Go calcula
ao vivo, e exporta pesos p/ signal.json. O Go faz score = w·(x-mean)/std em tempo real.
Reporta o IC do proxy (vs CatBoost) pra sabermos a perda."""
import json, numpy as np, pandas as pd
from sklearn.linear_model import Ridge
from mvp_perp_mm import build_day, TRAIN_DAYS, TEST_DAY

FEATS = ["micro_gap", "imb_c", "spread_bps", "ofi_5s", "sv_5s", "ret_1s"]

def feats(day):
    df = build_day(day).copy()
    r = np.log(df["mid"]).diff().fillna(0)
    df["imb_c"] = df["imbalance"] - 0.5
    df["ofi_5s"] = df["ofi_1"].rolling(50, 1).sum()      # 5s (50 bins) — igual à engine
    df["sv_5s"] = df["signed_vol"].rolling(50, 1).sum()
    df["ret_1s"] = r.rolling(10, 1).sum() * 1e4
    df["y"] = (np.log(df["mid"].shift(-10)) - np.log(df["mid"])) * 1e4
    return df.iloc[:-10].replace([np.inf, -np.inf], 0).fillna(0)

tr = pd.concat([feats(d) for d in TRAIN_DAYS], ignore_index=True)
te = feats(TEST_DAY)
mean = tr[FEATS].mean().values; std = tr[FEATS].std().replace(0, 1).values
Xtr = (tr[FEATS].values - mean) / std
Xte = (te[FEATS].values - mean) / std
m = Ridge(alpha=10.0).fit(Xtr, tr["y"].values)
p = m.predict(Xte); y = te["y"].values
idx = np.arange(0, len(y), 50)
print(f"proxy linear IC={np.corrcoef(p,y)[0,1]:.4f}  IC(indep50)={np.corrcoef(p[idx],y[idx])[0,1]:.4f}  (CatBoost era ~0.33)")
print("coefs:", dict(zip(FEATS, np.round(m.coef_, 4))))
json.dump({"feats": FEATS, "mean": mean.tolist(), "std": std.tolist(),
           "coef": m.coef_.tolist(), "intercept": float(m.intercept_)},
          open("signal.json", "w"), indent=2)
print("-> signal.json")
