"""
MVP: previsor de curtíssimo prazo p/ MM de perp (BTC-PERPETUAL, Deribit), treinado
nos dados Tardis (event-level). Pipeline:

  1. REPLAY do L2 incremental (mesma lógica da engine Go: book completo via deltas)
     em grid de 100ms -> best bid/ask, micro-price, imbalance, OFI (Cont-Kukanov-Stoikov).
  2. TRADES agregados por bin de 100ms (signed volume, contagem).
  3. ALVO = markout pontual: retorno do mid +1s à frente, em bps.
  4. 3 CatBoost walk-forward (treina dias antigos, testa o mais recente):
       - point (RMSE = média condicional)
       - q05 e q95 (Quantile) -> intervalo de 90%
  Sem conformal (amostra pequena). Foco no pontual; intervalo só medimos cobertura.

Uso: python mvp_perp_mm.py
"""
from __future__ import annotations
import csv, gzip, io, os
from pathlib import Path
import numpy as np
import pandas as pd

DATA = Path(__file__).parent / "tardis_data"
DAYS = ["2026-04-01", "2026-05-01", "2026-06-01"]  # têm L2 + trades
TRAIN_DAYS = DAYS[:-1]   # Abr + Mai
TEST_DAY = DAYS[-1]      # Jun (out-of-sample temporal)
BIN_US = 100_000         # 100ms em microssegundos
H_BINS = 10              # horizonte do alvo: +1s (10 bins de 100ms)


# ---------------------------------------------------------------- features
def build_day(day: str) -> pd.DataFrame:
    cache = DATA / f"feat_{day}.parquet"
    if cache.exists():
        return pd.read_parquet(cache)
    print(f"  [{day}] construindo features (replay L2)...")

    # --- trades agregados por bin de 100ms ---
    tr = pd.read_csv(DATA / f"trades_{day}.csv.gz")
    tr["bin"] = tr["timestamp"] // BIN_US
    tr["sv"] = np.where(tr["side"] == "buy", tr["amount"], -tr["amount"])
    tagg = tr.groupby("bin").agg(signed_vol=("sv", "sum"), ntr=("amount", "size"))

    # --- replay do L2 incremental ---
    bids: dict[float, float] = {}
    asks: dict[float, float] = {}
    prev_snap = False
    cur_bin = None
    pbP = pbS = paP = paS = 0.0  # best anterior (p/ OFI)
    rows = []

    def close_bin(b):
        nonlocal pbP, pbS, paP, paS
        if not bids or not asks:
            return
        bP = max(bids); bS = bids[bP]
        aP = min(asks); aS = asks[aP]
        if bP <= 0 or aP <= 0 or aP <= bP:
            return
        mid = (bP + aP) / 2
        micro = (aP * bS + bP * aS) / (bS + aS)
        # OFI best-level (Cont-Kukanov-Stoikov)
        if bP > pbP: eb = bS
        elif bP == pbP: eb = bS - pbS
        else: eb = -pbS
        if aP < paP: ea = aS
        elif aP == paP: ea = aS - paS
        else: ea = -paS
        pbP, pbS, paP, paS = bP, bS, aP, aS
        t = tagg.loc[b] if b in tagg.index else None
        rows.append((
            int(b), mid,
            (micro - mid) / mid * 1e4,          # micro_gap_bps
            bS / (bS + aS),                      # imbalance (best)
            (aP - bP) / mid * 1e4,               # spread_bps
            eb - ea,                             # ofi_1 (este bin)
            float(t.signed_vol) if t is not None else 0.0,
            int(t.ntr) if t is not None else 0,
        ))

    path = DATA / f"L2_{day}.csv.gz"
    with gzip.open(path, "rt") as f:
        rd = csv.reader(io.TextIOWrapper(f.buffer)) if hasattr(f, "buffer") else csv.reader(f)
        header = next(rd)
        ci = {name: k for k, name in enumerate(header)}
        iT, iS, iSide, iP, iA = ci["timestamp"], ci["is_snapshot"], ci["side"], ci["price"], ci["amount"]
        for r in rd:
            snap = r[iS] == "true"
            if snap and not prev_snap:   # novo snapshot -> reseta o book
                bids.clear(); asks.clear()
            prev_snap = snap
            ts = int(r[iT]); b = ts // BIN_US
            if cur_bin is None:
                cur_bin = b
            elif b != cur_bin:
                close_bin(cur_bin)
                cur_bin = b
            price = float(r[iP]); amt = float(r[iA])
            book = bids if r[iSide] == "bid" else asks
            if amt == 0:
                book.pop(price, None)
            else:
                book[price] = amt
        if cur_bin is not None:
            close_bin(cur_bin)

    df = pd.DataFrame(rows, columns=["bin", "mid", "micro_gap", "imbalance",
                                     "spread_bps", "ofi_1", "signed_vol", "ntr"])
    df = df.drop_duplicates("bin").sort_values("bin").reset_index(drop=True)
    df.to_parquet(cache, index=False)
    print(f"    {len(df):,} bins de 100ms")
    return df


def add_features_target(df: pd.DataFrame) -> pd.DataFrame:
    df = df.copy()
    r = np.log(df["mid"]).diff().fillna(0.0)             # retorno por bin
    df["ofi_5"] = df["ofi_1"].rolling(5, min_periods=1).sum()
    df["ofi_10"] = df["ofi_1"].rolling(10, min_periods=1).sum()
    df["sv_5"] = df["signed_vol"].rolling(5, min_periods=1).sum()
    df["sv_10"] = df["signed_vol"].rolling(10, min_periods=1).sum()
    df["ntr_5"] = df["ntr"].rolling(5, min_periods=1).sum()
    df["ret_10"] = (r.rolling(10, min_periods=1).sum()) * 1e4   # momentum 1s (bps)
    df["ret_30"] = (r.rolling(30, min_periods=1).sum()) * 1e4   # momentum 3s (bps)
    df["vol_30"] = (r.rolling(30, min_periods=2).std().fillna(0.0)) * 1e4
    # ALVO: retorno do mid +H bins à frente (markout pontual), bps
    fwd = np.log(df["mid"].shift(-H_BINS)) - np.log(df["mid"])
    df["y"] = fwd * 1e4
    return df.iloc[:-H_BINS]  # dropa as últimas (sem alvo)


FEATURES = ["micro_gap", "imbalance", "spread_bps", "ofi_1", "ofi_5", "ofi_10",
            "signed_vol", "sv_5", "sv_10", "ntr_5", "ret_10", "ret_30", "vol_30"]


# ---------------------------------------------------------------- treino
def main():
    print("Montando datasets...")
    parts = {d: add_features_target(build_day(d)) for d in DAYS}
    tr = pd.concat([parts[d] for d in TRAIN_DAYS], ignore_index=True)
    te = parts[TEST_DAY]
    Xtr, ytr = tr[FEATURES], tr["y"]
    Xte, yte = te[FEATURES], te["y"]
    print(f"treino: {len(tr):,} amostras ({'+'.join(TRAIN_DAYS)})  |  teste: {len(te):,} ({TEST_DAY})")
    print(f"alvo y (bps) treino: mean={ytr.mean():.3f} std={ytr.std():.2f}  "
          f"teste: mean={yte.mean():.3f} std={yte.std():.2f}")

    from catboost import CatBoostRegressor
    common = dict(iterations=600, learning_rate=0.03, depth=6, l2_leaf_reg=8.0,
                  random_seed=42, verbose=0)

    print("\nTreinando 3 modelos...")
    m_pt = CatBoostRegressor(loss_function="RMSE", **common).fit(Xtr, ytr)
    m_lo = CatBoostRegressor(loss_function="Quantile:alpha=0.05", **common).fit(Xtr, ytr)
    m_hi = CatBoostRegressor(loss_function="Quantile:alpha=0.95", **common).fit(Xtr, ytr)

    pt = m_pt.predict(Xte)
    lo = m_lo.predict(Xte)
    hi = m_hi.predict(Xte)

    # ---- avaliação do PONTUAL (o foco) ----
    err = pt - yte.values
    rmse = np.sqrt((err ** 2).mean())
    mae = np.abs(err).mean()
    mask = yte.values != 0
    diracc = (np.sign(pt[mask]) == np.sign(yte.values[mask])).mean()
    ic = np.corrcoef(pt, yte.values)[0, 1]
    ric = pd.Series(pt).corr(pd.Series(yte.values), method="spearman")
    base_rmse = np.sqrt((yte.values ** 2).mean())  # baseline prever 0
    print("\n=== PONTUAL (markout +1s, bps) — OOS Jun ===")
    print(f"  RMSE={rmse:.3f}  (baseline prever-0={base_rmse:.3f})  MAE={mae:.3f}")
    print(f"  IC (Pearson)={ic:.4f}  rankIC (Spearman)={ric:.4f}")
    print(f"  acerto direcional={diracc*100:.1f}%  (50% = moeda)")
    # IC por bucket de força do sinal (onde o modelo "tem certeza")
    q = pd.qcut(pd.Series(pt), 5, labels=False, duplicates="drop")
    by = pd.DataFrame({"pred": pt, "y": yte.values, "q": q}).groupby("q").agg(
        pred_mean=("pred", "mean"), y_mean=("y", "mean"), n=("y", "size"))
    print("  quintil do sinal -> retorno realizado médio (bps):")
    for qi, row in by.iterrows():
        print(f"    Q{int(qi)}: pred={row.pred_mean:+.3f}  realizado={row.y_mean:+.3f}  n={int(row.n)}")

    # ---- intervalo 90% (só reportar cobertura/largura) ----
    cov = ((yte.values >= lo) & (yte.values <= hi)).mean()
    width = (hi - lo).mean()
    crossed = (lo > hi).mean()
    print("\n=== INTERVALO 90% [q05,q95] — OOS Jun (sem conformal) ===")
    print(f"  cobertura empírica={cov*100:.1f}%  (alvo 90%)  largura média={width:.2f} bps  cruzados={crossed*100:.1f}%")

    print("\n=== importância das features (pontual) ===")
    imp = sorted(zip(FEATURES, m_pt.get_feature_importance()), key=lambda x: -x[1])
    for f, v in imp:
        print(f"  {f:12s} {v:5.1f}")

    Path("models").mkdir(exist_ok=True)
    m_pt.save_model("models/mvp_point.cbm")
    m_lo.save_model("models/mvp_q05.cbm")
    m_hi.save_model("models/mvp_q95.cbm")
    print("\nmodelos salvos em models/. (foco: o PONTUAL; intervalo é diagnóstico)")


if __name__ == "__main__":
    main()
