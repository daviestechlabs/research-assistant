FROM --platform=linux/amd64 golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd AS build
WORKDIR /work
RUN apt-get update && apt-get install -y --no-install-recommends \
    python3 libsqlite3-dev ca-certificates && rm -rf /var/lib/apt/lists/*
COPY *.go go.mod go.sum index.html manifest.webmanifest icon.svg sw.js graph.js desk.js quality.js library.js quality_cases.json quality_holdout.json quality_generated.json quality_generation.json quality_generation_audit.json books.json ./
COPY scripts/package-pdf-runtime.py scripts/package-pdf-runtime.py
RUN python3 scripts/package-pdf-runtime.py /work/pdf-runtime && \
    go test -race ./... && go vet ./... && \
    go build -trimpath -tags netgo,osusergo -ldflags '-linkmode external -extldflags "-static -lm -ldl -lpthread"' -o /work/research-assistant .
FROM scratch
COPY --from=build /work/pdf-runtime/root/ /
COPY --from=build /work/research-assistant /research-assistant
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 501:20
ENTRYPOINT ["/research-assistant"]
