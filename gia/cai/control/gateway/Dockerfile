FROM alpine:3.19
RUN apk add --no-cache ca-certificates

COPY control/gateway/carlos-gateway-linux /usr/local/bin/carlos-gateway
RUN chmod +x /usr/local/bin/carlos-gateway

EXPOSE 8001

CMD ["carlos-gateway"]
