FROM golang:1.25.7 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /copilot-proxy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /copilot-proxy /copilot-proxy
EXPOSE 8080
ENTRYPOINT ["/copilot-proxy"]
