FROM golang:1.26 AS builder

ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.buildVersion=${VERSION}" -o /vekil .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /vekil /vekil
EXPOSE 1337
ENTRYPOINT ["/vekil"]
