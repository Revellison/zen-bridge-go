FROM golang:1.27.3-bookworm AS build
WORKDIR /src
ARG VERSION=0.2.0
ARG COMMIT=unknown
ENV CGO_ENABLED=0 GOTOOLCHAIN=local
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN go build -buildvcs=false -trimpath -ldflags="-s -w -buildid= -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /out/zen-bridge ./cmd/zen-bridge \
    && mkdir -p /out/data && chown 10001:10001 /out/data && chmod 0700 /out/data

FROM scratch
WORKDIR /
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/zen-bridge /zen-bridge
COPY --from=build --chown=10001:10001 /out/data /data
USER 10001:10001
ENV GOGC=100
EXPOSE 18788
HEALTHCHECK --interval=15s --timeout=3s --start-period=5s --retries=3 CMD ["/zen-bridge", "healthcheck", "--url", "http://127.0.0.1:18788/healthz"]
ENTRYPOINT ["/zen-bridge"]
CMD ["serve", "--config", "/config/config.json"]
