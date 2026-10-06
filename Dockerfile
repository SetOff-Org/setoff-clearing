# setoff: the SetOff clearing service and CLI.
#
#   docker run -v /srv/setoff:/etc/setoff -e SETOFF_OPERATOR_KEY -e SETOFF_KEY_ANCHOR_NG ... \
#     -p 7500:7500 ghcr.io/setoff-org/setoff
#
# /etc/setoff holds setoff.toml and its data_dir. Set listen = "0.0.0.0:7500".
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /setoff ./cmd/setoff

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /setoff /setoff
WORKDIR /etc/setoff
EXPOSE 7500
ENTRYPOINT ["/setoff"]
CMD ["serve", "--config", "/etc/setoff/setoff.toml"]
