# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Bake the latest daily BTC/USD history into the binary. If the source is
# unreachable the build keeps the committed history file.
# Override with --build-arg PRICE_SOURCE=coinmetrics, or none to skip.
ARG PRICE_SOURCE=yahoo
RUN if [ "$PRICE_SOURCE" != "none" ]; then \
      go run ./cmd/btctax prices fetch -source "$PRICE_SOURCE" \
      || echo "warning: price fetch failed; using the committed price history"; \
    fi
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/btctax ./cmd/btctax \
 && mkdir -p /out/data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/btctax /btctax
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV BTCTAX_ADDR=0.0.0.0:8080 BTCTAX_DATA=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/btctax"]
