# deribit-microstructure

Infra de **medição** (sem estratégia) do mercado da Deribit, em Go. Lê o WebSocket
público (book + trades em streaming), mantém o livro ao vivo e calcula, por instrumento:

- **mid**, **micro-price** (mid ponderado pelo tamanho oposto — Stoikov), **spread (bps)**
- **imbalance** do topo do book (Qbid / (Qbid+Qask))
- **realized vol** anualizada (do mid)
- **κ (kappa)** e **A** do Avellaneda-Stoikov: ajusta `λ(d) = A·e^(−κd)` aos trades
  (distância `d` do mid em bps), por OLS log-linear → o decaimento da intensidade de
  fill conforme você se afasta do mid. **É o "k (bid/ask)".**

Objetivo: **testar nossa força em medir o mercado e o κ** antes de qualquer estratégia.
Read-only, sem API key, sem ordens. WS com heartbeat + **reconexão automática** (backoff).

## Rodar

```bash
go build -o deribit-micro.exe .
./deribit-micro.exe                 # abre http://localhost:8080
```

Flags:
- `-instruments "BTC-PERPETUAL,ETH-PERPETUAL"` — lista (csv)
- `-options=true` — auto-adiciona a CALL ATM ~30d de BTC e ETH (via REST)
- `-depth 10` — profundidade do book
- `-addr :8080` — porta

## Arquitetura

- `deribit.go` — cliente WS (1 conexão, N canais), parsing book/trades, REST p/ resolver
  a opção ATM, loop de reconexão. Book robusto a `[preço,tam]` e `["new",preço,tam]`.
- `metrics.go` — micro-price, imbalance, spread, RV, `fitKappa` (OLS), `Snapshot()`.
- `main.go` — servidor HTTP: `/` (dashboard) + `/api/metrics` (JSON, poll 1s).
- `index.html` — dashboard (vanilla JS + canvas, sem deps): cards por instrumento +
  sparklines de mid e spread (10 min).

## O que olhar

Contraste esperado (e a razão de opções ser jogo de modelo, não de latência):
perp com spread ~0,1 bps vs opção ~150-300 bps. O κ mede quão concentrada perto do
mid está a liquidez — input central de qualquer quoting de MM.
