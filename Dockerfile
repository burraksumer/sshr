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
# /data holds the host key, at .sshr/host_ed25519, and an optional
# sshr.toml. Settings come from that file or from SSHR_* variables, so the
# image sets no environment of its own.
WORKDIR /data
VOLUME /data
EXPOSE 2222
ENTRYPOINT ["/sshr"]
CMD ["serve"]
