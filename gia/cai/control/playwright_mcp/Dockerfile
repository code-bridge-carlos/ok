FROM mcr.microsoft.com/playwright/mcp:latest

ENV PORT=3000

ENTRYPOINT ["node", "/app/cli.js", "--headless", "--browser", "chromium", "--no-sandbox", "--host", "0.0.0.0", "--port", "3000"]
