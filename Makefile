BINARY := vekil
LDFLAGS := -s -w
VERSION ?= dev-$(shell git rev-parse --short HEAD)
APP_VERSION := $(patsubst v%,%,$(VERSION))

MACOS_CONFIG := build-support/macos/app-config.json
MACOS_BUILD_ROOT ?= .build/macos
MACOS_RELEASE_DIR ?= dist/macos-release
APP_NAME := Vekil.app
APP_BUNDLE_ID := com.vekil.menubar
MACOS_BUNDLE_VERSION ?=
MACOS_BUNDLE_BUILD_ID ?=
MACOS_RELEASE ?= 0

LEGACY_APP_NAME ?= Vekil-legacy.app
LEGACY_APP_EXECUTABLE := vekil-menubar
LEGACY_APP_ICON := assets/macos/Vekil.icns
LEGACY_RESOLVED_MANIFEST := $(MACOS_BUILD_ROOT)/legacy-release.json

TRAY_LINUX_BINARY := vekil-tray

.PHONY: \
	build test vet lint clean compaction-lab \
	build-app verify-app test-app smoke-app package-app fetch-sparkle \
	require-native-macos-sources macos-source-status test-macos-release-tools test-macos-appcast-generation test-macos-bundle-verification \
	build-legacy-app test-legacy-app build-tray-linux \
	docker-build docker-build-rtk docker-rtk-e2e

build:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) .

require-native-macos-sources:
	scripts/macos-native-source-status.sh --require

macos-source-status:
	scripts/macos-native-source-status.sh

fetch-sparkle:
	scripts/fetch-sparkle.sh >/dev/null

build-app: require-native-macos-sources
	VERSION="$(VERSION)" \
	MACOS_BUNDLE_VERSION="$(MACOS_BUNDLE_VERSION)" \
	MACOS_BUNDLE_BUILD_ID="$(MACOS_BUNDLE_BUILD_ID)" \
	MACOS_BUILD_ROOT="$(abspath $(MACOS_BUILD_ROOT))" \
	MACOS_APP_CONFIG="$(abspath $(MACOS_CONFIG))" \
	MACOS_RELEASE="$(MACOS_RELEASE)" \
	APP_PATH="$(abspath $(APP_NAME))" \
		scripts/build-macos-app.sh

verify-app:
	MACOS_APP_CONFIG="$(abspath $(MACOS_CONFIG))" \
	MACOS_RESOLVED_MANIFEST="$(abspath $(MACOS_BUILD_ROOT))/vekil-macos-release.json" \
	MACOS_RELEASE="$(MACOS_RELEASE)" \
	APP_PATH="$(abspath $(APP_NAME))" \
		scripts/verify-macos-app.sh

test-app: build-app verify-app

smoke-app: test-app
	MACOS_APP_CONFIG="$(abspath $(MACOS_CONFIG))" \
	MACOS_RESOLVED_MANIFEST="$(abspath $(MACOS_BUILD_ROOT))/vekil-macos-release.json" \
	MACOS_RELEASE="$(MACOS_RELEASE)" \
	APP_PATH="$(abspath $(APP_NAME))" \
		scripts/macos-app-smoke.sh

package-app: build-app
	MACOS_APP_CONFIG="$(abspath $(MACOS_CONFIG))" \
	MACOS_RESOLVED_MANIFEST="$(abspath $(MACOS_BUILD_ROOT))/vekil-macos-release.json" \
	MACOS_RELEASE_DIR="$(abspath $(MACOS_RELEASE_DIR))" \
	MACOS_RELEASE="$(MACOS_RELEASE)" \
	APP_PATH="$(abspath $(APP_NAME))" \
		scripts/package-macos-app.sh

# Keep the previous Go/systray shell buildable during the native rollback window.
# It is deliberately a separate target and never supplies the native release ZIP.
build-legacy-app: fetch-sparkle
	@set -e; \
	sparkle_root="$$(scripts/fetch-sparkle.sh)"; \
	framework="$$sparkle_root/$$(scripts/macos-release-manifest.py get --file "$(MACOS_CONFIG)" --key sparkle.framework_path)"; \
	minimum="$$(scripts/macos-release-manifest.py get --file "$(MACOS_CONFIG)" --key legacy_shell.minimum_system_version)"; \
	rm -rf "$(LEGACY_APP_NAME)"; \
	mkdir -p "$(LEGACY_APP_NAME)/Contents/MacOS" "$(LEGACY_APP_NAME)/Contents/Resources" "$(LEGACY_APP_NAME)/Contents/Frameworks" "$(MACOS_BUILD_ROOT)"; \
	VERSION="$(VERSION)" MACOS_BUNDLE_VERSION="$(MACOS_BUNDLE_VERSION)" \
		scripts/macos-release-manifest.py resolve --config "$(MACOS_CONFIG)" --output "$(LEGACY_RESOLVED_MANIFEST)"; \
	GOMODCACHE="$(abspath $(MACOS_BUILD_ROOT))/gomodcache" GOCACHE="$(abspath $(MACOS_BUILD_ROOT))/gocache" GOFLAGS=-mod=readonly \
	CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET="$$minimum" \
	CGO_CFLAGS="-mmacosx-version-min=$$minimum" \
	CGO_LDFLAGS="-F$$sparkle_root -Wl,-rpath,@executable_path/../Frameworks -mmacosx-version-min=$$minimum" \
		go build -tags sparkle -ldflags="$(LDFLAGS) -X main.buildVersion=$(APP_VERSION)" \
		-o "$(LEGACY_APP_NAME)/Contents/MacOS/$(LEGACY_APP_EXECUTABLE)" ./cmd/menubar/; \
	ditto "$(LEGACY_APP_ICON)" "$(LEGACY_APP_NAME)/Contents/Resources/Vekil.icns"; \
	ditto "$$framework" "$(LEGACY_APP_NAME)/Contents/Frameworks/Sparkle.framework"; \
	scripts/macos-release-manifest.py plist --legacy --manifest "$(LEGACY_RESOLVED_MANIFEST)" --output "$(LEGACY_APP_NAME)/Contents/Info.plist"; \
	codesign --force --deep --sign - --timestamp=none "$(LEGACY_APP_NAME)"; \
	codesign --verify --deep --strict "$(LEGACY_APP_NAME)"

test-legacy-app: build-legacy-app
	test -x "$(LEGACY_APP_NAME)/Contents/MacOS/$(LEGACY_APP_EXECUTABLE)"
	plutil -lint "$(LEGACY_APP_NAME)/Contents/Info.plist"
	/usr/libexec/PlistBuddy -c 'Print :CFBundleExecutable' "$(LEGACY_APP_NAME)/Contents/Info.plist" | grep -Fxq '$(LEGACY_APP_EXECUTABLE)'
	/usr/libexec/PlistBuddy -c 'Print :LSMinimumSystemVersion' "$(LEGACY_APP_NAME)/Contents/Info.plist" | grep -Fxq '10.13'
	otool -L "$(LEGACY_APP_NAME)/Contents/MacOS/$(LEGACY_APP_EXECUTABLE)" | grep -Fq '@rpath/Sparkle.framework/Versions/B/Sparkle'

build-tray-linux:
	CGO_ENABLED=0 GOOS=linux \
		go build -ldflags="$(LDFLAGS) -X main.buildVersion=$(APP_VERSION)" -o $(TRAY_LINUX_BINARY) ./cmd/menubar/

test:
	go test ./... -count=1

compaction-lab:
	go run ./cmd/compaction-lab

vet:
	go vet ./...

lint: vet

test-macos-release-tools:
	scripts/tests/macos-release-tools-test.sh

test-macos-appcast-generation:
	scripts/tests/macos-appcast-generation-test.sh

test-macos-bundle-verification:
	scripts/tests/macos-bundle-verification-test.sh

clean:
	rm -f $(BINARY) $(TRAY_LINUX_BINARY)
	rm -rf "$(APP_NAME)" "$(LEGACY_APP_NAME)" .build "$(MACOS_RELEASE_DIR)"

docker-build:
	docker build -t $(BINARY) .

docker-build-rtk:
	docker build -f Dockerfile.rtk -t $(BINARY):rtk .

docker-rtk-e2e:
	scripts/rtk-container-e2e.sh
