# Multi-stage build: a static Go binary on an empty base image. The runtime
# image contains only the binary and CA certificates — no shell, no package
# manager, no browser auth helpers, and never any credentials.
FROM docker.io/library/golang:1.24-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/freedeep ./cmd/freedeep

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/freedeep /freedeep

ENV HOST=0.0.0.0 \
    PORT=9655 \
    NON_INTERACTIVE=1 \
    DEEPSEEK_AUTH_PATH=/run/secrets/deepseek-auth.json \
    PROXY_API_KEY_FILE=/run/secrets/proxy-api-key \
    REQUIRE_PROXY_API_KEY=1

USER 1000:1000

EXPOSE 9655

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/freedeep", "healthcheck"]

ENTRYPOINT ["/freedeep"]
CMD ["serve"]
