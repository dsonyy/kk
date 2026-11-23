export PATH := env_var("PATH") + ":/snap/bin"

default:
    @just --list

# Install web dependencies when package.json changed
web-deps:
    cd web && ([ node_modules -nt package.json ] || npm install)

# Build the web client and the kk binary with the client embedded
build: web-deps
    cd web && npm run build
    go build -o kk ./app

# Run the test suite
test: build
    go test ./...

# Build, install to ~/.local/bin and restart the kk user service
install: build
    install -m 755 kk ~/.local/bin/kk.new && mv ~/.local/bin/kk.new ~/.local/bin/kk
    mkdir -p ~/.config/systemd/user
    cp kk.service ~/.config/systemd/user/kk.service
    systemctl --user daemon-reload
    systemctl --user enable kk.service
    systemctl --user restart kk.service
