.PHONY: build

build:
	go build -trimpath -o ./mirror ./cmd/mirror
