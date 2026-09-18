FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/smagent ./cmd/smagent

FROM alpine:3.21
COPY --from=build /out/smagent /usr/local/bin/smagent
ENTRYPOINT ["smagent"]
