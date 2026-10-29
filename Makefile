.PHONY: build-go run-go seed-go reset-go bench-go test-go clean-go
.PHONY: clean

# Go targets
build-go:
	$(MAKE) -C go build

run-go:
	$(MAKE) -C go run

seed-go:
	$(MAKE) -C go seed

reset-go:
	$(MAKE) -C go reset

bench-go:
	$(MAKE) -C go bench

test-go:
	$(MAKE) -C go test

clean-go:
	$(MAKE) -C go clean

clean: clean-go
