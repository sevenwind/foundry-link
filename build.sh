#!/bin/bash
# Собирает Foundry Link.app рядом со скриптом
set -e
cd "$(dirname "$0")"
swift build -c release
APP="Foundry Link.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS"
cp .build/release/FoundryLink "$APP/Contents/MacOS/FoundryLink"
cat > "$APP/Contents/Info.plist" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleName</key><string>Foundry Link</string>
<key>CFBundleIdentifier</key><string>local.foundrylink.app</string>
<key>CFBundleExecutable</key><string>FoundryLink</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>1.0</string>
<key>CFBundleVersion</key><string>1</string>
<key>LSMinimumSystemVersion</key><string>14.0</string>
<key>NSHighResolutionCapable</key><true/>
<key>NSAppTransportSecurity</key><dict><key>NSAllowsLocalNetworking</key><true/><key>NSAllowsArbitraryLoads</key><true/></dict>
</dict></plist>
PL
codesign --force --sign - "$APP" >/dev/null 2>&1 || true
echo "Готово: $APP"
