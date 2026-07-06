FROM golang:1.26 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=
RUN VERSION_NO_V=${VERSION#v} && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X github.com/sozercan/vekil/proxy.metricsBuildVersion=${VERSION_NO_V} -X github.com/sozercan/vekil/proxy.metricsBuildCommit=${COMMIT}" -o /vekil .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /vekil /vekil
ENV HOST=0.0.0.0
EXPOSE 1337
ENTRYPOINT ["/vekil"]
