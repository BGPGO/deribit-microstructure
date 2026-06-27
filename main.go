// deribit-microstructure: infra de MEDIÇÃO (sem estratégia).
// Lê o WebSocket público da Deribit (book + trades), mantém o livro ao vivo e
// calcula mid / micro-price / spread / imbalance / realized vol / kappa (A-S).
// Frontend embutido em / ; dados em /api/metrics (JSON).
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strings"
)

//go:embed index.html
var webFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "endereço HTTP")
	insts := flag.String("instruments", "BTC-PERPETUAL,ETH-PERPETUAL", "instrumentos Deribit (csv)")
	depth := flag.Int("depth", 10, "profundidade do book")
	withOpts := flag.Bool("options", true, "auto-adiciona opção ATM ~30d de BTC e ETH")
	selftest := flag.Bool("selftest", false, "TESTNET: auth+place+cancel (lê DERIBIT_CLIENT_ID/SECRET do env) e sai")
	mmInst := flag.String("mm", "", "TESTNET: roda MM preliminar neste instrumento (dashboard :8081). vazio=desliga")
	mmSecs := flag.Int("mmsecs", 0, "duração do MM em segundos (0=indefinido)")
	flag.Parse()

	if *selftest {
		runOrderSelfTest()
		return
	}
	if *mmInst != "" {
		runMM(*mmInst, *mmSecs)
		return
	}

	var list []string
	for _, s := range strings.Split(*insts, ",") {
		if s = strings.TrimSpace(s); s != "" {
			list = append(list, s)
		}
	}
	if *withOpts {
		for _, ccy := range []string{"BTC", "ETH"} {
			if name, err := resolveATMOption(ccy, 30); err == nil && name != "" {
				list = append(list, name)
				log.Printf("opção ATM ~30d %s resolvida: %s", ccy, name)
			} else {
				log.Printf("aviso: não resolvi opção %s (%v) — seguindo sem ela", ccy, err)
			}
		}
	}

	sig := loadSigModel("signal.json")
	if sig != nil {
		log.Printf("sinal carregado (proxy linear, %d features)", len(sig.Feats))
	} else {
		log.Printf("aviso: signal.json não encontrado — shadow-signal desligado")
	}
	volM := loadSigModel("vol.json")
	if volM != nil {
		log.Printf("previsor de vol carregado (proxy linear HAR)")
	}
	mgr := NewManager(list, *depth, sig, volM)
	go mgr.RunForever()
	go mgr.RunVolLoop()

	http.HandleFunc("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mgr.Snapshot())
	})
	http.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mgr.Events())
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := webFS.ReadFile("index.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
	})
	log.Printf("servindo em http://localhost%s  | instrumentos: %v", *addr, list)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
