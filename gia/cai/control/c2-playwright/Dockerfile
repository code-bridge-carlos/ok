FROM mcr.microsoft.com/playwright:v1.51.0-noble

WORKDIR /app

# Copy package files
COPY package*.json ./

# Install dependencies
RUN npm ci --only=production

# Copy runner
COPY runner.js ./

# Create user data dir
RUN mkdir -p /home/pwuser/.config/deepseek-browser && chown -R pwuser:pwuser /home/pwuser

USER pwuser

ENV C2_WS=wss://carlos-gateway.onrender.com/c2/ws
ENV USER_DATA_DIR=/home/pwuser/.config/deepseek-browser

CMD ["node", "runner.js"]