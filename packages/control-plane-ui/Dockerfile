FROM node:20-alpine AS builder
WORKDIR /app
COPY package.json package-lock.json ./
COPY packages/control-plane-ui/package.json packages/control-plane-ui/
RUN npm ci --workspace=packages/control-plane-ui
COPY packages/control-plane-ui packages/control-plane-ui
RUN cd packages/control-plane-ui && npm run build

FROM nginx:alpine
COPY --from=builder /app/packages/control-plane-ui/dist /usr/share/nginx/html
COPY packages/control-plane-ui/nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 3000
