BINARY := vekil
LDFLAGS := -s -w
APP_NAME := Vekil.app
APP_BUNDLE_ID := com.vekil.menubar
VERSION ?= dev-$(shell git rev-parse --short HEAD)
APP_VERSION := $(patsubst v%,%,$(VERSION))
SPARKLE_VERSION := 2.9.0
SPARKLE_BUILD_DIR := .build/sparkle
SPARKLE_ARCHIVE := $(SPARKLE_BUILD_DIR)/Sparkle-$(SPARKLE_VERSION).tar.xz
SPARKLE_UNPACK_DIR := $(SPARKLE_BUILD_DIR)/unpacked
SPARKLE_FRAMEWORK := $(SPARKLE_UNPACK_DIR)/Sparkle.framework
SPARKLE_DOWNLOAD_URL := https://github.com/sparkle-project/Sparkle/releases/download/$(SPARKLE_VERSION)/Sparkle-$(SPARKLE_VERSION).tar.xz
SPARKLE_FEED_URL ?= https://github.com/sozercan/vekil/releases/latest/download/appcast.xml
SPARKLE_PUBLIC_ED_KEY ?=

.PHONY: build build-app test vet lint clean docker-build

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

$(SPARKLE_ARCHIVE):
	@mkdir -p "$(SPARKLE_BUILD_DIR)"
	curl -fL "$(SPARKLE_DOWNLOAD_URL)" -o "$(SPARKLE_ARCHIVE)"

$(SPARKLE_FRAMEWORK): $(SPARKLE_ARCHIVE)
	@rm -rf "$(SPARKLE_UNPACK_DIR)"
	@mkdir -p "$(SPARKLE_UNPACK_DIR)"
	tar -xf "$(SPARKLE_ARCHIVE)" -C "$(SPARKLE_UNPACK_DIR)"

build-app: $(SPARKLE_FRAMEWORK)
	@rm -rf "$(APP_NAME)"
	@mkdir -p "$(APP_NAME)/Contents/MacOS"
	@mkdir -p "$(APP_NAME)/Contents/Resources"
	@mkdir -p "$(APP_NAME)/Contents/Frameworks"
	CGO_ENABLED=1 CGO_LDFLAGS="-F$(abspath $(SPARKLE_UNPACK_DIR))" \
		go build -tags sparkle -ldflags="$(LDFLAGS) -X main.buildVersion=$(APP_VERSION)" -o "$(APP_NAME)/Contents/MacOS/vekil-menubar" ./cmd/menubar/
	ditto "$(SPARKLE_FRAMEWORK)" "$(APP_NAME)/Contents/Frameworks/Sparkle.framework"
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
	<key>LSMinimumSystemVersion</key>\n\
	<string>10.13</string>\n\
	<key>LSUIElement</key>\n\
	<true/>\n\
	<key>SUEnableInstallerLauncherService</key>\n\
	<true/>\n\
	<key>SUFeedURL</key>\n\
	<string>$(SPARKLE_FEED_URL)</string>\n\
	<key>SUPublicEDKey</key>\n\
	<string>$(SPARKLE_PUBLIC_ED_KEY)</string>\n\
	<key>SURequireSignedFeed</key>\n\
	<true/>\n\
	<key>SUVerifyUpdateBeforeExtraction</key>\n\
	<true/>\n\
</dict>\n\
</plist>' > "$(APP_NAME)/Contents/Info.plist"
	codesign --force --deep --sign - --timestamp=none "$(APP_NAME)"
	codesign --verify --deep --strict "$(APP_NAME)"

test:
	go test ./... -count=1

vet:
	go vet ./...

lint: vet

clean:
	rm -f $(BINARY)
	rm -rf "$(APP_NAME)" .build

docker-build:
	docker build -t $(BINARY) .
