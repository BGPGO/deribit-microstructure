"""Babilônia de features: multi-escala (OFI, fluxo, RV, momentum, range, dispersão,
EWMA, interações, hora-do-dia/ciclo funding). Mede se MELHORA OOS os 2 alvos
(direção 1s, vol 30s) vs o conjunto enxuto. Juiz = OOS, não importância."""
from __future__ import annotations
import numpy as np, pandas as pd
from catboost import CatBoostRegressor
from mvp_perp_mm import build_day, TRAIN_DAYS, TEST_DAY

WINS = [2, 5, 10, 20, 50, 100, 300, 600, 1200, 3000]  # bins de 100ms: 0.2s..5min
BASE13 = ["micro_gap", "imbalance", "spread_bps", "ofi_1", "ofi_5", "ofi_10",
          "signed_vol", "sv_5", "sv_10", "ntr_5", "ret_10", "ret_30", "vol_30"]


def big_features(day):
    df = build_day(day).copy()
    r = np.log(df["mid"]).diff().fillna(0.0)
    df["r"] = r
    f = {}
    # enxutas originais (mesma definição p/ baseline justo)
    f["ofi_5"] = df["ofi_1"].rolling(5, min_periods=1).sum()
    f["ofi_10"] = df["ofi_1"].rolling(10, min_periods=1).sum()
    f["sv_5"] = df["signed_vol"].rolling(5, min_periods=1).sum()
    f["sv_10"] = df["signed_vol"].rolling(10, min_periods=1).sum()
    f["ntr_5"] = df["ntr"].rolling(5, min_periods=1).sum()
    f["ret_10"] = r.rolling(10, min_periods=1).sum() * 1e4
    f["ret_30"] = r.rolling(30, min_periods=1).sum() * 1e4
    f["vol_30"] = r.rolling(30, min_periods=2).std().fillna(0) * 1e4
    # BABILÔNIA multi-escala
    for w in WINS:
        f[f"ofi_{w}"] = df["ofi_1"].rolling(w, min_periods=1).sum()
        f[f"sv_{w}"] = df["signed_vol"].rolling(w, min_periods=1).sum()
        f[f"ntr_{w}"] = df["ntr"].rolling(w, min_periods=1).sum()
        f[f"rv_{w}"] = np.sqrt((r ** 2).rolling(w, min_periods=2).sum()) * 1e4
        f[f"ret_{w}"] = r.rolling(w, min_periods=1).sum() * 1e4
        f[f"imbm_{w}"] = df["imbalance"].rolling(w, min_periods=1).mean()
        f[f"imbs_{w}"] = df["imbalance"].rolling(w, min_periods=2).std().fillna(0)
        f[f"mgapm_{w}"] = df["micro_gap"].rolling(w, min_periods=1).mean()
        f[f"sprdm_{w}"] = df["spread_bps"].rolling(w, min_periods=1).mean()
        f[f"sprds_{w}"] = df["spread_bps"].rolling(w, min_periods=2).std().fillna(0)
        f[f"rng_{w}"] = (df["mid"].rolling(w, min_periods=1).max() -
                         df["mid"].rolling(w, min_periods=1).min()) / df["mid"] * 1e4
    # dinâmica / EWMA
    for hl in [5, 20, 100, 500]:
        f[f"ofi_ewm{hl}"] = df["ofi_1"].ewm(halflife=hl).mean()
        f[f"imb_ewm{hl}"] = df["imbalance"].ewm(halflife=hl).mean()
        f[f"r_ewm{hl}"] = r.ewm(halflife=hl).mean() * 1e4
    f["imb_d"] = df["imbalance"].diff().fillna(0)
    f["mgap_d"] = df["micro_gap"].diff().fillna(0)
    f["ofi_d"] = df["ofi_1"].diff().fillna(0)
    # interações / normalizações
    f["mgap_over_sprd"] = df["micro_gap"] / (df["spread_bps"] + 1e-6)
    f["imb_x_sprd"] = df["imbalance"] * df["spread_bps"]
    f["avg_signed_sz"] = df["signed_vol"] / (df["ntr"] + 1)
    f["ofi_over_rv"] = df["ofi_1"] / (r.rolling(50, min_periods=2).std().fillna(0) * 1e4 + 1e-3)
    # hora-do-dia / ciclo funding 8h (literatura: spikes pós-settlement 02/10/18 UTC)
    sec = (df["bin"] * 0.1)
    f["hour"] = (sec / 3600) % 24
    f["fund8h"] = (sec % (8 * 3600)) / (8 * 3600)
    F = pd.DataFrame(f)
    out = pd.concat([df[["bin", "mid", "micro_gap", "imbalance", "spread_bps", "ofi_1",
                         "signed_vol", "ntr"]], F], axis=1)
    # alvos
    out["y_dir"] = (np.log(out["mid"].shift(-10)) - np.log(out["mid"])) * 1e4
    out["y_vol"] = np.log(np.sqrt((r ** 2).rolling(300).sum().shift(-300)) * 1e4 + 0.01)
    return out.iloc[:-300].replace([np.inf, -np.inf], 0).fillna(0)


def main():
    print("Montando babilônia de features...")
    parts = {d: big_features(d) for d in TRAIN_DAYS + [TEST_DAY]}
    tr = pd.concat([parts[d] for d in TRAIN_DAYS], ignore_index=True)
    te = parts[TEST_DAY]
    allf = [c for c in tr.columns if c not in ("bin", "mid", "y_dir", "y_vol")]
    print(f"total de features: {len(allf)}  | treino {len(tr):,} teste {len(te):,}")
    common = dict(iterations=700, learning_rate=0.03, depth=6, l2_leaf_reg=10, random_seed=42, verbose=0)

    def train_eval(feats, target, tag, vol=False):
        m = CatBoostRegressor(loss_function="RMSE", **common).fit(tr[feats], tr[target])
        p = m.predict(te[feats]); y = te[target].values
        if vol:
            r2 = 1 - np.sum((y - p) ** 2) / np.sum((y - y.mean()) ** 2)
            print(f"  [{tag}] feats={len(feats)}  R2(log)={r2:.4f}  corr={np.corrcoef(p,y)[0,1]:.4f}")
        else:
            idx = np.arange(0, len(y), 50); mask = y[idx] != 0
            da = (np.sign(p[idx][mask]) == np.sign(y[idx][mask])).mean()
            print(f"  [{tag}] feats={len(feats)}  IC={np.corrcoef(p,y)[0,1]:.4f}  IC(indep50)={np.corrcoef(p[idx],y[idx])[0,1]:.4f}  dir={da*100:.1f}%")
        return m

    print("\n=== DIREÇÃO (markout 1s) ===")
    train_eval(BASE13, "y_dir", "enxuto 13")
    md = train_eval(allf, "y_dir", "BABILÔNIA")
    print("\n=== VOL (30s) ===")
    base_vol = BASE13 + [f"rv_{w}" for w in (50, 300, 600, 3000)]
    train_eval(base_vol, "y_vol", "enxuto+HAR", vol=True)
    mv = train_eval(allf, "y_vol", "BABILÔNIA", vol=True)

    for m, tag in [(md, "DIREÇÃO"), (mv, "VOL")]:
        print(f"\n=== top-12 importância [{tag}] ===")
        for f, v in sorted(zip(allf, m.get_feature_importance()), key=lambda x: -x[1])[:12]:
            print(f"  {f:14s} {v:.1f}")


if __name__ == "__main__":
    main()
