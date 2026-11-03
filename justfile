build:
    go build -o eufy-server ./cmd/server
    go build -o eufy-cli ./cmd/cli

test:
    go test ./...

# lint runs vet plus a dead-code sweep. deadcode is pinned in go.mod as a
# tool dependency (go get -tool golang.org/x/tools/cmd/deadcode).
lint:
    go vet ./...
    go tool deadcode -test ./...
