#!/bin/bash
# Собирает Foundry Link:
#   dist/Foundry Link.app            — macOS (универсальное: Apple Silicon + Intel), окно + вшитый движок
#   dist/FoundryLink-macOS.zip       — то же в архиве для отправки
#   dist/FoundryLink-Windows.zip     — Windows (FoundryLink.exe + README)
# Нужны: Go 1.23+, Xcode Command Line Tools (swiftc, lipo, iconutil, codesign).
set -euo pipefail
cd "$(dirname "$0")"
VERSION=$(grep -m1 'const version' main.go | cut -d'"' -f2)
rm -rf dist build && mkdir -p dist build

echo "==> Go: движок для macOS и Windows"
LD="-s -w"
GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags "$LD" -o build/flink-arm64 .
GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags "$LD" -o build/flink-amd64 .
lipo -create build/flink-arm64 build/flink-amd64 -output build/flink
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LD" -o build/FoundryLink.exe .

echo "==> Swift: окно macOS"
swiftc -O -target arm64-apple-macos13  -o build/shell-arm64 mac/main.swift -framework Cocoa -framework WebKit
swiftc -O -target x86_64-apple-macos13 -o build/shell-amd64 mac/main.swift -framework Cocoa -framework WebKit
lipo -create build/shell-arm64 build/shell-amd64 -output build/FoundryLink

echo "==> Сборка Foundry Link.app"
APP="dist/Foundry Link.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp build/FoundryLink "$APP/Contents/MacOS/FoundryLink"
cp build/flink "$APP/Contents/Resources/flink"
cp mac/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"
cat > "$APP/Contents/Info.plist" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleName</key><string>Foundry Link</string>
<key>CFBundleDisplayName</key><string>Foundry Link</string>
<key>CFBundleIdentifier</key><string>local.foundrylink.p2p</string>
<key>CFBundleExecutable</key><string>FoundryLink</string>
<key>CFBundleIconFile</key><string>AppIcon</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>CFBundleShortVersionString</key><string>$VERSION</string>
<key>CFBundleVersion</key><string>$VERSION</string>
<key>LSMinimumSystemVersion</key><string>13.0</string>
<key>NSHighResolutionCapable</key><true/>
<key>NSAppTransportSecurity</key><dict><key>NSAllowsLocalNetworking</key><true/><key>NSAllowsArbitraryLoads</key><true/></dict>
</dict></plist>
PL
codesign --force --deep --sign - "$APP" >/dev/null 2>&1 || echo "предупреждение: ad-hoc подпись не удалась"
(cd dist && ditto -c -k --keepParent "Foundry Link.app" FoundryLink-macOS.zip)

echo "==> Windows-архив"
mkdir -p build/win && cp build/FoundryLink.exe build/win/
cat > build/win/ПРОЧТИ-МЕНЯ.txt <<TXT
Foundry Link $VERSION для Windows

1. Запустите FoundryLink.exe. Откроется чёрное окно (не закрывайте его: это и есть программа)
   и интерфейс в браузере. Если браузер не открылся, зайдите на http://127.0.0.1:17630
2. Если Windows показывает "Система Windows защитила ваш компьютер": «Подробнее» → «Выполнить в любом случае».
3. Если Windows спросит про брандмауэр, нажмите «Разрешить доступ».
4. Мастер: выберите «Я мастер», отправьте игрокам код комнаты, вставьте их ответные коды.
   Игрок: выберите «Я игрок», вставьте код комнаты, отправьте мастеру ответный код,
   затем нажмите «Открыть Foundry».
5. Не включайте VPN во время игры: он мешает прямому соединению.
Выйти: кнопка «Выйти» в интерфейсе или закрытие чёрного окна.
TXT
(cd build/win && zip -q -r ../../dist/FoundryLink-Windows.zip .)

echo "Готово:"; ls -la dist
