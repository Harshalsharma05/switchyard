# Two stages: build the Vite app with the Node toolchain, then serve the static
# output from nginx. nginx is also the single production origin — it proxies
# /v1 to the gateway's public port and /admin to its admin port, so the app
# keeps calling same-origin relative paths exactly as it does behind the Vite
# dev proxy.

FROM node:22-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build

FROM nginx:1.27-alpine AS final
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
