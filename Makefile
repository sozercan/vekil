BINARY := copilot-proxy
LDFLAGS := -s -w

.PHONY: build build-menubar test vet lint clean docker-build

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

build-menubar:
	go build -ldflags="$(LDFLAGS)" -o copilot-proxy-menubar ./cmd/menubar/

test:
	go test ./... -count=1

vet:
	go vet ./...

lint: vet

clean:
	rm -f $(BINARY) copilot-proxy-menubar

docker-build:
	docker build -t $(BINARY) .
