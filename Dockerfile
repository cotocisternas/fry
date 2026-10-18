# Images resolved from official registries on 2026-10-17; see container research.
FROM --platform=$BUILDPLATFORM golang:1.27.2-trixie@sha256:47667934874913a449b96faa7ef4777fa285d9ec29126ee80ed61c45b7cccf0e AS build
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
COPY LICENSE ./
COPY third_party/ ./third_party/
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY db/ ./db/
COPY gen/ ./gen/
RUN mkdir -p /out/bin /out/data /out/licenses && chmod 0700 /out/data && \
    GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -mod=readonly -trimpath -buildvcs=false -o /out/bin/ ./cmd/fry ./cmd/fry-member ./cmd/fry-migrate ./cmd/fry-admin
# Preserve dependency notices alongside the binaries, including local patches.
RUN go list -m -f '{{.Path}} {{if .Replace}}{{.Replace.Dir}}{{else}}{{.Dir}}{{end}}' all | \
    while read -r module source; do \
      [ -n "$source" ] || continue; \
      mkdir -p "/out/licenses/$module"; \
      for notice in "$source"/LICENSE "$source"/LICENSE.* "$source"/COPYING "$source"/NOTICE "$source"/NOTICE.* "$source"/AUTHORS; do \
        [ ! -f "$notice" ] || cp "$notice" "/out/licenses/$module/"; \
      done; \
    done

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/bin/ /usr/local/bin/
COPY --from=build /out/licenses/ /usr/share/fry/licenses/
COPY --from=build --chown=65532:65532 --chmod=0700 /out/data/ /var/lib/fry/
USER 65532:65532
WORKDIR /var/lib/fry
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/fry"]
CMD ["-help"]
