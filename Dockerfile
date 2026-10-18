# Packaging only: scripts/container.sh builds and verifies these outputs natively.
# The index pin also supplies TLS roots and the matching amd64/arm64 runtime.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
ARG TARGETARCH
COPY --chmod=0555 runtime/linux-${TARGETARCH}/bin/fry runtime/linux-${TARGETARCH}/bin/fry-member runtime/linux-${TARGETARCH}/bin/fry-migrate runtime/linux-${TARGETARCH}/bin/fry-admin /usr/local/bin/
COPY runtime/linux-${TARGETARCH}/licenses/ /usr/share/fry/licenses/
COPY --chown=65532:65532 --chmod=0700 runtime/linux-${TARGETARCH}/data/ /var/lib/fry/
USER 65532:65532
WORKDIR /var/lib/fry
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/fry"]
CMD ["-help"]
