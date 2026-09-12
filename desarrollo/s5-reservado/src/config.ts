export const config = {
  port: Number(Bun.env.PORT ?? 9005),
  s1Url: Bun.env.S1_URL ?? "http://localhost:8080",
  controlToken: Bun.env.CONTROL_TOKEN ?? "dev-token-b4",
};
