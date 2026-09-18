# --- front ---
FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- hub ---
FROM golang:1.26-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/build ./internal/hub/webdist/build
RUN CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=${VERSION}" -o /out/smhub ./cmd/smhub

# --- runtime ---
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/smhub /smhub
ENV SM_LISTEN=:8090 SM_DATA_DIR=/data
VOLUME /data
EXPOSE 8090
ENTRYPOINT ["/smhub"]
