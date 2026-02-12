BINARY := copilot-proxy
LDFLAGS := -s -w

.PHONY: build test vet lint clean docker-build

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./... -count=1

vet:
	go vet ./...

lint: vet

clean:
	rm -f $(BINARY)

docker-build:
	docker build -t $(BINARY) .
