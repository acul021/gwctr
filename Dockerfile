FROM golang:1.23-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gwctr ./

FROM alpine:3.20
RUN apk add --no-cache iptables ip6tables
COPY --from=build /out/gwctr /usr/local/bin/gwctr
ENTRYPOINT ["/usr/local/bin/gwctr"]
