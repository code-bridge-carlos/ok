FROM oven/bun:1-slim
WORKDIR /app
COPY package.json bun.lock ./
RUN bun install --frozen-lockfile --production
COPY . .
ENV PORT=9003
EXPOSE 9003
CMD ["bun", "run", "src/index.ts"]
