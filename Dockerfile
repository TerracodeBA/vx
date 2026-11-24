# vx-server and the vx CLI in one small image.
#   docker build -t vx .
#   docker run -p 7070:7070 -e VX_DATABASE_URL=... vx
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/TerracodeBA/vx/internal/version.Version=${VERSION} -X github.com/TerracodeBA/vx/internal/version.Commit=${COMMIT}" \
      -o /out/ ./cmd/vx ./cmd/vx-server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates openssh-client tzdata \
 && adduser -D -h /home/vx -u 10001 vx \
 && mkdir -p /data && chown vx /data
COPY --from=build /out/vx /out/vx-server /usr/local/bin/
USER vx
ENV VX_ADDR=0.0.0.0:7070 VX_DATA_DIR=/data
VOLUME /data
EXPOSE 7070
HEALTHCHECK --interval=15s --timeout=3s CMD wget -qO- http://127.0.0.1:7070/healthz || exit 1
ENTRYPOINT ["vx-server"]
CMD ["serve"]
