"""Previsor de VOL de curtíssimo prazo (realized vol do mid nos próximos 1s, bps).
Testa se os quantis do previsor direcional (q05,q95,largura) ajudam como features.
Treina BASE (só microestrutura) vs AUG (+ quantis) e compara OOS. Loss em log-RV.
"""
from __future__ import annotations
import numpy as np, pandas as pd
from catboost import CatBoostRegressor
from mvp_perp_mm import build_day, add_features_target, FEATURES, DAYS, TRAIN_DAYS, TEST_DAY, H_BINS

EPS = 0.01  # bps, p/ log


def prep(day, mq05, mq95):
    df = add_features_target(build_day(day))
    # quantis do previsor direcional como features (OOS no dia de teste)
    df["q05"] = mq05.predict(df[FEATURES])
    df["q95"] = mq95.predict(df[FEATURES])
    df["qwidth"] = df["q95"] - df["q05"]
    # alvo: realized vol do mid nos próximos H bins (bps)
    r = np.log(df["mid"]).diff().fillna(0.0)
    fwd_sumr2 = (r ** 2).rolling(H_BINS).sum().shift(-H_BINS)
    df["rv"] = np.sqrt(fwd_sumr2) * 1e4
    df["logrv"] = np.log(df["rv"] + EPS)
    return df.dropna(subset=["rv"])


def main():
    mq05 = CatBoostRegressor().load_model("models/mvp_q05.cbm")
    mq95 = CatBoostRegressor().load_model("models/mvp_q95.cbm")
    parts = {d: prep(d, mq05, mq95) for d in DAYS}
    tr = pd.concat([parts[d] for d in TRAIN_DAYS], ignore_index=True)
    te = parts[TEST_DAY]

    base_f = FEATURES
    aug_f = FEATURES + ["q05", "q95", "qwidth"]
    common = dict(iterations=600, learning_rate=0.03, depth=6, l2_leaf_reg=8.0, random_seed=42, verbose=0)

    print(f"treino {len(tr):,} | teste {len(te):,} ({TEST_DAY})")
    print(f"RV alvo (bps) teste: mean={te['rv'].mean():.3f} std={te['rv'].std():.3f} mediana={te['rv'].median():.3f}")

    def fit_eval(feats, tag):
        m = CatBoostRegressor(loss_function="RMSE", **common).fit(tr[feats], tr["logrv"])
        pred_log = m.predict(te[feats])
        pred = np.exp(pred_log) - EPS
        y = te["rv"].values
        ic = np.corrcoef(pred, y)[0, 1]                          # corr em nível
        ic_log = np.corrcoef(pred_log, te["logrv"].values)[0, 1] # corr em log
        ss = 1 - np.sum((te["logrv"].values - pred_log) ** 2) / np.sum((te["logrv"].values - te["logrv"].mean()) ** 2)
        mae = np.abs(pred - y).mean()
        print(f"\n[{tag}]  corr(nível)={ic:.4f}  corr(log)={ic_log:.4f}  R²(log)={ss:.4f}  MAE={mae:.3f} bps")
        return m

    mb = fit_eval(base_f, "BASE  (só microestrutura)")
    ma = fit_eval(aug_f, "AUG   (+ q05,q95,largura)")

    print("\n=== importância (AUG) — onde caem os quantis ===")
    for f, v in sorted(zip(aug_f, ma.get_feature_importance()), key=lambda x: -x[1]):
        star = " <--" if f in ("q05", "q95", "qwidth") else ""
        print(f"  {f:12s} {v:5.1f}{star}")


if __name__ == "__main__":
    main()
