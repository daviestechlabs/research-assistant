SQLITE_PREFIX ?= $(if $(wildcard /opt/homebrew/opt/sqlite/include/sqlite3.h),/opt/homebrew/opt/sqlite,)
ifneq ($(strip $(SQLITE_PREFIX)),)
export CGO_CFLAGS := $(CGO_CFLAGS) -I$(SQLITE_PREFIX)/include
export CGO_LDFLAGS := $(CGO_LDFLAGS) -L$(SQLITE_PREFIX)/lib
endif

.PHONY: all test check clean
all:
	go build -trimpath -o research-assistant .
test:
	go test -race ./...
check:
	go vet ./...
clean:
	rm -f research-assistant
