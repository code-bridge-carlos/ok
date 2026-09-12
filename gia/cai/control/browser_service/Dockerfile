FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    chromium \
    fonts-freefont-ttf \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ENV CHROME_PATH=/usr/bin/chromium
ENV PULSE_SERVER=nonexistent

COPY control/browser_service/browser-service-linux /usr/local/bin/browser-service
RUN chmod +x /usr/local/bin/browser-service

EXPOSE 8080

CMD ["browser-service"]
