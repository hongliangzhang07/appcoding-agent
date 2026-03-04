.PHONY: run build release

run:
	go run .

build:
	go build -o appcoding-agent .

release:
	./scripts/release-pack.sh
