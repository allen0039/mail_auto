# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/mail-auto ./cmd/mail-auto
RUN mkdir -p /state/data /state/keys && chmod 0700 /state/data /state/keys

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/mail-auto /usr/local/bin/mail-auto
COPY --from=build --chown=65532:65532 /state /var/lib/mail-auto
COPY THIRD_PARTY_NOTICES.txt /usr/share/doc/mail-auto/THIRD_PARTY_NOTICES.txt
WORKDIR /var/lib/mail-auto
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/mail-auto"]
CMD ["--listen", "0.0.0.0:8080", "serve"]
