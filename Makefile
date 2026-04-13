BINARY := vekil
LDFLAGS := -s -w
APP_NAME := Vekil.app
APP_BUNDLE_ID := com.vekil.menubar
VERSION ?= dev-$(shell git rev-parse --short HEAD)
APP_VERSION := $(patsubst v%,%,$(VERSION))

.PHONY: build build-app test vet lint clean docker-build

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

build-app:
	@rm -rf "$(APP_NAME)"
	@mkdir -p "$(APP_NAME)/Contents/MacOS"
	@mkdir -p "$(APP_NAME)/Contents/Resources"
	go build -ldflags="$(LDFLAGS) -X main.buildVersion=$(APP_VERSION)" -o "$(APP_NAME)/Contents/MacOS/vekil-menubar" ./cmd/menubar/
	@printf '<?xml version="1.0" encoding="UTF-8"?>\n\
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n\
<plist version="1.0">\n\
<dict>\n\
	<key>CFBundleExecutable</key>\n\
	<string>vekil-menubar</string>\n\
	<key>CFBundleIdentifier</key>\n\
	<string>$(APP_BUNDLE_ID)</string>\n\
	<key>CFBundleName</key>\n\
	<string>Vekil</string>\n\
	<key>CFBundlePackageType</key>\n\
	<string>APPL</string>\n\
	<key>CFBundleVersion</key>\n\
	<string>$(APP_VERSION)</string>\n\
	<key>CFBundleShortVersionString</key>\n\
	<string>$(APP_VERSION)</string>\n\
	<key>LSUIElement</key>\n\
	<true/>\n\
</dict>\n\
</plist>' > "$(APP_NAME)/Contents/Info.plist"

test:
	go test ./... -count=1

vet:
	go vet ./...

lint: vet

clean:
	rm -f $(BINARY)
	rm -rf "$(APP_NAME)"

docker-build:
	docker build -t $(BINARY) .
