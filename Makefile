FILES = $(go list ./... | grep -v /vendor/)

all: gotling

gotling:
	go build

.PHONY: build
build: gotling

.PHONY: lintcheck
lintcheck:
	gometalinter --vendor --deadline 10m --config gometalinter.json $(FILES)

.PHONY: test
test:
	go test -timeout 30s $(FILES)

.PHONY: clean
clean:
	rm gotling

.PHONY: install
install:
	go install
