.PHONY: build run seed reset bench test test-api test-bdd test-race clean

build:
	go build -o bin/server ./cmd/server

run:
	go run ./cmd/server

seed:
	go run ./cmd/server --seed

reset:
	rm -rf data
	go run ./cmd/server --seed

bench:
	@mkdir -p data
	go test -bench=. -run=^$$ 2>data/bench.log

test:
	go test ./...

# Native HTTP + SQLite scenarios create their own credentials and temporary data.
test-api:
	go test -count=1 ./internal/api ./internal/app ./cmd/server

test-bdd:
	go test -count=1 -run '^TestBDD' -v ./internal/app ./cmd/server ./internal/seed

test-race:
	go test -race -count=1 ./...

clean:
	rm -rf bin data
