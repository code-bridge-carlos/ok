FROM oven/bun:1-slim
WORKDIR /app
COPY package.json bun.lock* ./
RUN bun install --frozen-lockfile --production 2>/dev/null || bun install --production
COPY . .
ENV PORT=9005
EXPOSE 9005
CMD ["bun", "run", "src/index.ts"]
