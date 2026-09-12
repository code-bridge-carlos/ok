export const config = {
  port: Number(Bun.env.PORT ?? 9003),
  databaseUrl: Bun.env.DATABASE_URL ?? "postgres://postgres:test@localhost:5433/testdb?sslmode=disable",
  s1Url: Bun.env.S1_URL ?? "http://localhost:8080",
  controlToken: Bun.env.CONTROL_TOKEN ?? "dev-token-b4",
  engramUrl: Bun.env.ENGRAM_URL ?? "http://127.0.0.1:7437",
  engramBin: Bun.env.ENGRAM_BIN ?? "/home/codespace/.local/bin/engram",
};
