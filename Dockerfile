# syntax=docker/dockerfile:1
# Three images from the same source: the app people run (the default
# target), the public demo (--target demo) and the landing page (--target
# site). Building one never builds the others, and the app's image carries
# none of the demo's code or libraries.
#
# Multi-arch (amd64 and arm64, for Umbrel and StartOS boxes): Go runs on the
# build machine's own platform and cross-compiles for the target, and the
# final stages only copy files in, so an arm64 image needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
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
ARG TARGETOS TARGETARCH
# The release tag (v1.2.3) the app reports, or "dev".
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/btctax ./cmd/btctax

FROM build AS build-demo
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/btctax-demo ./cmd/btctax-demo

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

# The landing page: site/ (with its own screenshots, taken without the demo
# bar) and the app's font and icon, behind nginx. Static files only; it
# never talks to the app.
FROM nginx:1.27-alpine AS site
COPY site/ /usr/share/nginx/html/
COPY web/static/fonts/ /usr/share/nginx/html/fonts/
COPY web/static/favicon.svg /usr/share/nginx/html/favicon.svg

# The app (the default target, so it stays last). On a small standard base
# rather than scratch: StartOS runs the image's own filesystem and expects
# what a Linux system has (/proc and /tmp to mount into, /etc/passwd to look
# the user up in), and Alpine also brings the CA certificates.
FROM alpine:3.24 AS app
COPY --from=build-app /out/btctax /btctax
COPY --from=build --chown=65532:65532 /out/data /data
USER 65532:65532
ENV BTCTAX_ADDR=0.0.0.0:8080 BTCTAX_DATA=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/btctax"]
