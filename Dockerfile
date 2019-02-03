# build stage
FROM golang:alpine as build-env
RUN apk update && apk upgrade && apk add git
RUN go get github.com/tools/godep
COPY . /go/src/github.com/gopher-net/docker-ovs-plugin
WORKDIR /go/src/github.com/gopher-net/docker-ovs-plugin
RUN godep go build -v -o docker-ovs-plugin

# final stage
FROM alpine
RUN apk update && apk upgrade && apk add iptables dbus
WORKDIR /app
COPY --from=build-env /go/src/github.com/gopher-net/docker-ovs-plugin/docker-ovs-plugin /app/
ENTRYPOINT ["/app/docker-ovs-plugin"]
