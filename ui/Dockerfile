FROM node:26.13.1-alpine3.24 AS base

WORKDIR /app

FROM base AS builder

ENV CI=true

# Install dependencies first so this layer stays cached until the lockfile
# changes. Node 26 ships no corepack, so install the pnpm that package.json's
# packageManager pins; it stays in this stage only. The split drops a
# `+sha512.` suffix, which npm can't parse.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
RUN npm install -g "$(node -p 'require("./package.json").packageManager.split("+")[0]')" --no-audit --no-fund && \
    pnpm install --frozen-lockfile --prod=false --ignore-scripts

COPY tsconfig.json vite.config.ts react-router.config.ts tailwind.config.js postcss.config.js ./
COPY app/ ./app/

RUN pnpm run build && \
    pnpm prune --prod --ignore-scripts

FROM base AS runtime

RUN apk add --no-cache --upgrade 'ca-certificates>=20261020-r0'
RUN ! grep -q MIICTjCCAdOgAwIBAgIKPPYHqWhwDtqLhDAKBggqhkjOPQQDAzBrMQswCQYDVQQG /etc/ssl/certs/ca-certificates.crt

COPY --from=builder /app/package.json ./
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/build ./build

EXPOSE 3000
ENV NODE_ENV=production

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q --spider http://localhost:3000/health || exit 1

CMD ["node_modules/.bin/react-router-serve", "build/server/index.js"]
