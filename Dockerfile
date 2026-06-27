FROM golang:1.25-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY *.go index.html ./
RUN CGO_ENABLED=0 go build -o deribit-micro .

FROM alpine:latest
WORKDIR /app
RUN apk add --no-cache ca-certificates
COPY --from=build /app/deribit-micro .
COPY signal.json vol.json ./
EXPOSE 8080
# shadow-MM live nos 6 perps (gateado pelo sinal, vol-sized, markout +5s, 30% haircut)
CMD ["./deribit-micro", "-addr", ":8080", "-options=false", "-instruments", "BTC-PERPETUAL,ETH-PERPETUAL,SOL_USDC-PERPETUAL,XRP_USDC-PERPETUAL,DOGE_USDC-PERPETUAL,BNB_USDC-PERPETUAL"]
