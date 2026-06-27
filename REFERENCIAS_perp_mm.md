# Fazer o MM de perp funcionar — Reading List + Síntese (verificada)

Pesquisa de 4 frentes (jun/2026) sobre como tornar lucrativo o MM num perp de spread fino,
para um quant **model-driven, não-co-located (semi-pro)**. O shadow-MM mostrou: cotar no BBO
PERDE (seleção adversa > spread). A literatura diz por quê — e qual é o único caminho.

## ⭐ A referência decisiva (é o NOSSO experimento, com o fix)
**Albers, Cucuringu, Howison, Shestopaloff (Oxford, 2025) — "The Market Maker's Dilemma: Fill Probability vs. Post-Fill Returns Trade-Off"** · arXiv 2502.18625 · https://arxiv.org/html/2502.18625v2
232.897 ordens maker REAIS em BTC perp da Binance (fev/2024): MM ingênuo no topo do book **perdeu ~60% em ~3,7 dias APESAR do rebate** (Sharpe anual −109). FIX: prever **reversões** com modelo sobre microestrutura e postar **contra-imbalance** (alta prob. de fill + retorno pós-fill positivo). É exatamente o que medimos, com a solução.

## 1. Controle ótimo (A-S / GLFT / Cartea) — o esqueleto do quoting
- **Avellaneda & Stoikov 2008** — inventory model base. https://people.orie.cornell.edu/sfs33/LimitOrderBook.pdf
- **Stoikov 2018 — The Micro-Price** — fair value = mid ajustado por imbalance+spread, martingale, melhor preditor curto que o mid. **Cotar em torno do micro-price, NÃO do mid.** SSRN 2970694.
- **Guéant, Lehalle, Fernández-Tapia 2013 (GLFT)** — A-S tratável (ODEs lineares): quote = half-spread + skew linear no inventário. arXiv 1105.3115.
- **Guéant 2017 "Optimal market making"** + livro 2016 (CRC) — fórmulas fechadas; half-spread em função de `k` (decaimento da intensidade). arXiv 1605.01862.
- **Cartea, Jaimungal, Penalva 2015** (livro, Cambridge) — penalidade quadrática de inventário (running penalty) > restrição terminal: melhor p/ book always-on (perp).
- **Cartea, Donnelly, Jaimungal 2018 — "Enhancing trading strategies with order book signals"** — imbalance prevê sinal da próxima market order; embutido no quoting **corta seleção adversa**. ORA Oxford uuid:006addde-3a03-4d75-89c1-04b59026e1c0.
- **Cartea & Wang 2020 — "Market Making with Alpha Signals"** — alpha de momentum entra como drift no value function; quotes inclinam na direção do sinal ALÉM do skew de inventário. ORA uuid:c2ba6656-...
- **Barzykin, Bergault, Guéant, Lemmel 2025 — "Optimal Quoting under Adverse Selection and Price Reading"** · arXiv 2508.20225 — quoting que trata fluxo informado E vazamento do próprio skew ("skew sniffers"). Tese-chave: **sofisticação de modelo compensa parcialmente desvantagem de velocidade** (contra fluxo de horizonte mais longo; contra speed-informed só resta alargar/recolher).
- **Le 2026 — "Funding-Aware Optimal MM for Perpetual DEXs"** · arXiv 2605.06405 — funding como estado estocástico acoplado ao inventário; skew ótimo enviesa pro lado que o funding PAGA. Edge único de perp, calibrado em Hyperliquid.

## 2. Sinais de microestrutura (o que alimenta o skew preditivo)
- **Cont, Kukanov, Stoikov 2014 — Order-Flow Imbalance (OFI)** — OFI (fluxo líquido, não volume) é o driver linear do retorno curto; impacto ∝ 1/profundidade. arXiv 1011.6402.
- **Kolm, Turiel, Westray 2023 — "Deep Order Flow Imbalance"** — modelos sobre OFI batem book cru; **horizonte efetivo ~2 price changes** (alpha vive em horizonte curtíssimo). Math. Finance 33(4).
- **Gould & Bonart 2016 — Queue Imbalance one-tick-ahead** — imbalance do topo prevê direção do próximo tick; mais forte em large-tick. arXiv 1512.03492.
- **Easley, López de Prado, O'Hara 2012 — VPIN/toxicidade** — RFS 25(5). ⚠️ **LER A CRÍTICA**: Andersen & Bondarenko 2014 (JFM) — VPIN é largamente mecânico, atrasa. Usar markout próprio como gate de toxicidade, VPIN só corroborando.
- **Huang, Lehalle, Rosenbaum 2015 — Queue-Reactive Model** — simulador LOB state-dependent p/ testar táticas. arXiv 1312.0563.

## 3. ML/RL (veredito: pragmático = A-S forecast-skewed com CatBoost, NÃO RL)
- **Gašperov et al. 2021 (Mathematics, MDPI)** — survey RL-MM. Começar aqui pro mapa.
- **Wang 2025 — "Better Inputs Matter More Than Stacking Another Hidden Layer"** · arXiv 2506.05764 — em LOB de cripto (Bybit BTC), **XGBoost/logística batem DeepLOB**; feature/denoise > profundidade. **Joga a favor do CatBoost.**
- **Falces Marin et al. 2022 (PLOS ONE)** — RL afina o γ do A-S sobre 30d de BTC/USD real, bate A-S tunado por GA. Padrão: RL como *wrapper* de tuning, não fonte do edge. Código: github.com/javifalces/HFTFramework.
- **Jiang et al. 2025 — "Relaver"** · arXiv 2505.12465 — RL-MM anterior **ignora latência → inaplicável**; fix = latência 30-100ms + previsão no estado.
- **Jafree, Jain, Firoozye 2025** · arXiv 2510.27334 — incluir fills adversos na sim é ESSENCIAL; excluir gera "ganhos fantasma".
- **DeepLOB (Zhang, Zohren, Roberts 2019)** arXiv 1808.03668 · **Jha et al. 2020** (Coinbase BTC, 71% @2s) arXiv 2010.01241 · **LOBCAST (Briola 2024)** arXiv 2308.01915 — crítica: SOTA despenca fora do FI-2010.

## 4. Economia: rebates & viabilidade (veredito: rebate NÃO salva; gated)
- Rebates maker: Binance/Bybit/OKX top-VIP chegam a ~0 ou levemente negativo, mas **−0,005%/−0,015% via MM program exige $100M+/30d + acordo bilateral** (não-retail). **Deribit: 0% maker / 0,05% taker + rebate −0,01% no perp aberto a todos.** Hyperliquid: share-based −0,001%..−0,003% (precisa ≥0,5% do volume maker total).
- **Glosten-Milgrom 1985** — spread nasce PURO de seleção adversa. **Stoll 1989** — adverse selection ~43% do spread; realized spread = quoted − adverse selection.
- **Menkveld 2013** — HFT-MM real **perde no inventário, ganha no spread**, ~80% passivo. SSRN 1722924.
- **Multicoin "Adverse Selection Rules Everything Around Me" (2026)** — cripto precificado como se todo trader fosse informado.
- **Amberdata (Marshall 2025)** — imbalance ~0 de poder preditivo a 1min em BTC (corr 0,011); **o sinal naive está morto, precisa de modelo de verdade**.
- **Execução realista:** hftbacktest (queue-aware + markout) — https://hftbacktest.readthedocs.io — OBRIGATÓRIO antes de capital. HangukQuant markout analysis.
- **Latência:** crypto não tem co-lo real; mesma região AWS (Binance=Tokyo, Deribit=London). <50ms→82% fill, >150ms→31%. Floor ~$50-100k capital + ~$600-1500/mo infra.

---

## SÍNTESE — a receita convergente (o único caminho pro semi-pro)

Naive BBO MM perde (provado, é o resultado de livro). Rebate não salva. **O único edge de um player lento é PREDITIVO**, e a forma é:

1. **Centrar no micro-price** (Stoikov), não no mid. Upgrade mais barato e robusto.
2. **Skew de inventário** (A-S/GLFT, running penalty) — base controller.
3. **Skew de ALPHA via CatBoost de horizonte curto — prevendo REVERSÃO / postando CONTRA-IMBALANCE** (fix do Oxford), não seguindo imbalance naive (que está morto). Features: OFI multi-nível, gap do micro-price, trade-sign. Trees ≥ deep nets em cripto LOB.
4. **Tilt de funding** (Le 2026) — edge único de perp, soma ao alpha.
5. **Gate de toxicidade** por **markout** (retorno pós-fill) próprio — alargar/recolher quando tóxico.
6. **Backtest queue-aware + latência + markout** (hftbacktest) ANTES de qualquer capital. Fills otimistas mentem.

**Venue:** majors são dos HFT co-located; superfície do semi-pro = alts mais finas ou Hyperliquid (HLP/sub-vaults). Mesmo DEX tem corrida de latência (~200ms Tokyo).

**Honestidade:** vira menos "spread capture" e mais "alpha direcional de horizonte curto que usa ordens passivas". Se o edge preditivo não sobreviver ao markout líquido de custo → a resposta colapsa pra NÃO.
