# syntax=docker/dockerfile:1
# Two images from the same source: the app people run (the default target)
# and the public demo (--target demo). Building one never builds the other,
# and the app's image carries none of the demo's code or libraries.
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Bake the latest daily BTC/USD history into the binary: the committed file
# plus the days since its last one. If the source is unreachable the build
# keeps the committed history as it is.
# Override with --build-arg PRICE_SOURCE=coinmetrics, or none to skip.
ARG PRICE_SOURCE=yahoo
RUN if [ "$PRICE_SOURCE" != "none" ]; then \
      go run ./cmd/btctax prices fetch -source "$PRICE_SOURCE" -from last \
      || echo "warning: price fetch failed; using the committed price history"; \
    fi
RUN mkdir -p /out/data

FROM build AS build-app
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/btctax ./cmd/btctax

FROM build AS build-demo
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/btctax-demo ./cmd/btctax-demo

# The public demo: invented data, a private copy per visitor, the assistant
# on Claude in Amazon Bedrock. Nothing is kept between restarts.
FROM scratch AS demo
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build-demo /out/btctax-demo /btctax-demo
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV BTCTAX_ADDR=0.0.0.0:8080 BTCTAX_DATA=/data
EXPOSE 8080
ENTRYPOINT ["/btctax-demo"]

# The app (the default target, so it stays last).
FROM scratch AS app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build-app /out/btctax /btctax
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV BTCTAX_ADDR=0.0.0.0:8080 BTCTAX_DATA=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/btctax"]
