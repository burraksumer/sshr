FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /sshr ./cmd/sshr \
 && mkdir -p /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /sshr /sshr
COPY --from=build --chown=nonroot:nonroot /data /data
ENV SSHR_LISTEN=:2222 \
    SSHR_HOST_KEY=/data/host_ed25519 \
    SSHR_CONFIG=/etc/sshr/sshr.toml
VOLUME /data
EXPOSE 2222
ENTRYPOINT ["/sshr"]
CMD ["serve"]
