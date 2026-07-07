FROM golang:1.26 AS builder

ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /vekil .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /vekil /vekil
ENV HOST=0.0.0.0
EXPOSE 1337
ENTRYPOINT ["/vekil"]
