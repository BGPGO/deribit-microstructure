package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"

	"github.com/gorilla/websocket"
)

const wsTest = "wss://test.deribit.com/ws/api/v2"

// runOrderSelfTest: prova o pipeline de ordem no TESTNET (auth -> place -> cancel).
// Lê DERIBIT_CLIENT_ID / DERIBIT_CLIENT_SECRET do ambiente. Nunca hardcoded.
// Ordem é limit post_only 5% abaixo do mark -> NÃO preenche (zero risco), só testa o round-trip.
func runOrderSelfTest() {
	id := os.Getenv("DERIBIT_CLIENT_ID")
	sec := os.Getenv("DERIBIT_CLIENT_SECRET")
	if id == "" || sec == "" {
		log.Fatal("defina DERIBIT_CLIENT_ID e DERIBIT_CLIENT_SECRET no ambiente (PowerShell: $env:DERIBIT_CLIENT_ID=...)")
	}
	c, _, err := websocket.DefaultDialer.Dial(wsTest, nil)
	if err != nil {
		log.Fatal("dial testnet: ", err)
	}
	defer c.Close()

	idc := 0
	rpc := func(method string, params map[string]any) (json.RawMessage, error) {
		idc++
		myid := idc
		if err := c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": myid, "method": method, "params": params}); err != nil {
			return nil, err
		}
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return nil, err
			}
			var resp struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
					Code    int    `json:"code"`
				} `json:"error"`
			}
			if json.Unmarshal(data, &resp) != nil || resp.ID != myid {
				continue // ignora subscriptions/heartbeats e respostas de outro id
			}
			if resp.Error != nil {
				return nil, fmt.Errorf("%s: %s (code %d)", method, resp.Error.Message, resp.Error.Code)
			}
			return resp.Result, nil
		}
	}

	// 1) auth (a sessão WS fica autenticada após isto)
	if _, err := rpc("public/auth", map[string]any{
		"grant_type": "client_credentials", "client_id": id, "client_secret": sec}); err != nil {
		log.Fatal("auth: ", err)
	}
	log.Println("✓ autenticado no testnet")

	// 2) mark price
	r, err := rpc("public/ticker", map[string]any{"instrument_name": "BTC-PERPETUAL"})
	if err != nil {
		log.Fatal("ticker: ", err)
	}
	var tk struct {
		MarkPrice float64 `json:"mark_price"`
	}
	_ = json.Unmarshal(r, &tk)
	price := math.Round(tk.MarkPrice*0.95*2) / 2 // 5% abaixo, arredondado p/ tick 0.5
	log.Printf("mark=%.1f -> buy post_only em %.1f (não cruza)", tk.MarkPrice, price)

	// 3) place
	r, err = rpc("private/buy", map[string]any{
		"instrument_name": "BTC-PERPETUAL", "amount": 10, "type": "limit",
		"price": price, "post_only": true, "label": "selftest"})
	if err != nil {
		log.Fatal("buy: ", err)
	}
	var ord struct {
		Order struct {
			OrderID string `json:"order_id"`
			State   string `json:"order_state"`
		} `json:"order"`
	}
	_ = json.Unmarshal(r, &ord)
	log.Printf("✓ ordem colocada: id=%s state=%s", ord.Order.OrderID, ord.Order.State)

	// 4) confirma no book
	r, _ = rpc("private/get_open_orders_by_instrument", map[string]any{"instrument_name": "BTC-PERPETUAL"})
	var open []json.RawMessage
	_ = json.Unmarshal(r, &open)
	log.Printf("✓ %d ordem(ns) aberta(s) no book", len(open))

	// 5) cancel
	if _, err := rpc("private/cancel", map[string]any{"order_id": ord.Order.OrderID}); err != nil {
		log.Fatal("cancel: ", err)
	}
	log.Println("✓ ordem cancelada — round-trip auth/place/cancel OK no testnet 🎉")
}
