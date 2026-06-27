"""
MVP FINAL: features enxutas (focado + gemas validadas) -> modelos direção/vol ->
teste de MAKER MARKOUT condicionado a fill (naive vs gateado pelo sinal), com rebate.
O teste econômico que decide se o perp tem edge de MM pra um semi-pro.
"""
from __future__ import annotations
import numpy as np, pandas as pd, gzip
from catboost import CatBoostRegressor
from mvp_perp_mm import build_day, TRAIN_DAYS, TEST_DAY, DATA

BIN_US = 100_000
REBATE_BPS = 1.0   # Deribit perp maker rebate 0.01% = 1bp por fill
MK = [10, 50]      # horizontes de markout: 1s, 5s


def lean(day):
    df = build_day(day).copy()
    r = np.log(df["mid"]).diff().fillna(0.0); r2 = r ** 2
    # ---- features DIREÇÃO (enxuto + gemas: imb_x_sprd, ofi_ewm5) ----
    df["ofi_5"] = df["ofi_1"].rolling(5, 1).sum()
    df["ofi_10"] = df["ofi_1"].rolling(10, 1).sum()
    df["sv_5"] = df["signed_vol"].rolling(5, 1).sum()
    df["sv_10"] = df["signed_vol"].rolling(10, 1).sum()
    df["ntr_5"] = df["ntr"].rolling(5, 1).sum()
    df["ret_10"] = r.rolling(10, 1).sum() * 1e4
    df["ret_30"] = r.rolling(30, 1).sum() * 1e4
    df["vol_30"] = r.rolling(30, 2).std().fillna(0) * 1e4
    df["imb_x_sprd"] = df["imbalance"] * df["spread_bps"]      # gema
    df["ofi_ewm5"] = df["ofi_1"].ewm(halflife=5).mean()        # gema
    # ---- features VOL (HAR + time, as gemas) ----
    for w in (50, 300, 600, 3000):
        df[f"rv_{w}"] = np.sqrt(r2.rolling(w, 2).sum()) * 1e4
    df["sprds_3000"] = df["spread_bps"].rolling(3000, 2).std().fillna(0)
    df["rng_3000"] = (df["mid"].rolling(3000, 1).max() - df["mid"].rolling(3000, 1).min()) / df["mid"] * 1e4
    sec = df["bin"] * 0.1
    df["hour"] = (sec / 3600) % 24                             # gema
    df["fund8h"] = (sec % 28800) / 28800                       # gema (ciclo funding 8h)
    # quote aproximado do BBO a partir de mid + spread
    hs = df["mid"] * (df["spread_bps"] / 1e4) / 2
    df["bid"] = df["mid"] - hs; df["ask"] = df["mid"] + hs
    # alvos
    df["y_dir"] = (np.log(df["mid"].shift(-10)) - np.log(df["mid"])) * 1e4
    df["y_vol"] = np.log(np.sqrt(r2.rolling(300).sum().shift(-300)) * 1e4 + 0.01)
    return df.iloc[:-300].replace([np.inf, -np.inf], 0).fillna(0)


DIR_FOCUS = ["micro_gap", "imbalance", "spread_bps", "ofi_1", "ofi_5", "ofi_10",
             "signed_vol", "sv_5", "sv_10", "ntr_5", "ret_10", "ret_30", "vol_30"]
DIR_GEMS = DIR_FOCUS + ["imb_x_sprd", "ofi_ewm5"]
VOL_FOCUS = ["spread_bps", "imbalance", "vol_30", "rv_50", "rv_300", "rv_600", "rv_3000", "sprds_3000", "rng_3000"]
VOL_GEMS = VOL_FOCUS + ["hour", "fund8h"]
CB = dict(iterations=700, learning_rate=0.03, depth=6, l2_leaf_reg=10, random_seed=42, verbose=0)


def main():
    parts = {d: lean(d) for d in TRAIN_DAYS + [TEST_DAY]}
    tr = pd.concat([parts[d] for d in TRAIN_DAYS], ignore_index=True)
    te = parts[TEST_DAY]

    # ---- validação das gemas (OOS) ----
    def ic_indep(feats, tgt):
        m = CatBoostRegressor(loss_function="RMSE", **CB).fit(tr[feats], tr[tgt])
        p = m.predict(te[feats]); y = te[tgt].values; idx = np.arange(0, len(y), 50)
        return np.corrcoef(p[idx], y[idx])[0, 1], m
    def r2_(feats, tgt):
        m = CatBoostRegressor(loss_function="RMSE", **CB).fit(tr[feats], tr[tgt])
        p = m.predict(te[feats]); y = te[tgt].values
        return 1 - np.sum((y - p) ** 2) / np.sum((y - y.mean()) ** 2), m
    print("=== validação das gemas (OOS Jun) ===")
    ic0, _ = ic_indep(DIR_FOCUS, "y_dir"); ic1, m_dir = ic_indep(DIR_GEMS, "y_dir")
    print(f"  direção IC(indep): focado={ic0:.4f} -> +gemas={ic1:.4f}")
    v0, _ = r2_(VOL_FOCUS, "y_vol"); v1, m_vol = r2_(VOL_GEMS, "y_vol")
    print(f"  vol R²(log):       focado={v0:.4f} -> +gemas={v1:.4f}")

    # modelos finais (direção: point + quantis)
    m_lo = CatBoostRegressor(loss_function="Quantile:alpha=0.05", **CB).fit(tr[DIR_GEMS], tr["y_dir"])
    m_hi = CatBoostRegressor(loss_function="Quantile:alpha=0.95", **CB).fit(tr[DIR_GEMS], tr["y_dir"])
    te = te.reset_index(drop=True)
    te["pred"] = m_dir.predict(te[DIR_GEMS])

    # ============ MVP: MAKER MARKOUT (naive vs gateado) ============
    # trades do dia de teste -> por bin: menor preço de venda, maior preço de compra
    tdf = pd.read_csv(DATA / f"trades_{TEST_DAY}.csv.gz")
    tdf["bin"] = tdf["timestamp"] // BIN_US
    sell = tdf[tdf.side == "sell"].groupby("bin")["price"].min()   # cruza nosso bid
    buy = tdf[tdf.side == "buy"].groupby("bin")["price"].max()     # cruza nosso ask
    bin2row = {b: i for i, b in enumerate(te["bin"].values)}
    mid = te["mid"].values; bid = te["bid"].values; ask = te["ask"].values
    pred = te["pred"].values; bins = te["bin"].values
    n = len(te)

    def run(gated):
        # fills: (row_do_fill, side(+1 long/-1 short), entry_px)
        fills = []
        for i in range(n - 1):
            nb = bins[i] + 1
            if nb not in bin2row:
                continue
            j = bin2row[nb]  # bin seguinte
            post_bid = (not gated) or pred[i] > 0   # gateado: posta bid se prevê alta
            post_ask = (not gated) or pred[i] < 0   # posta ask se prevê queda
            if post_bid and nb in sell.index and sell.loc[nb] <= bid[i]:
                fills.append((j, +1, bid[i]))
            if post_ask and nb in buy.index and buy.loc[nb] >= ask[i]:
                fills.append((j, -1, ask[i]))
        res = {}
        for H in MK:
            mks = []
            for j, s, entry in fills:
                if j + H < n:
                    mks.append(s * (mid[j + H] - entry) / entry * 1e4)
            mks = np.array(mks)
            res[H] = (len(mks), mks.mean(), (mks + REBATE_BPS).mean(), (mks + REBATE_BPS).sum())
        return res

    print(f"\n=== MAKER MARKOUT — OOS Jun (rebate +{REBATE_BPS}bp/fill) ===")
    for tag, gated in [("NAIVE (posta os 2 lados)", False), ("SINAL (gateia pelo lado previsto)", True)]:
        r = run(gated)
        print(f"  {tag}")
        for H in MK:
            nf, gross, net, tot = r[H]
            print(f"    markout +{H//10}s: fills={nf:>7,}  bruto={gross:+.3f}bp  líquido(+reb)={net:+.3f}bp  total={tot:+.1f}bp")


if __name__ == "__main__":
    main()
