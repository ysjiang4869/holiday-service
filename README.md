# 我的休息日历

基于 https://github.com/paulzhn/chinese-holiday-api 开发，保留原项目 Git 历史和原 API 源码；实际服务入口为新的 `main.go`，原 `api.Handler` 不再注册。公共数据来自 https://github.com/NateScarlet/holiday-cn 。

手机和电脑可用的日历、每用户独立请假/加班标记、查询 Token，以及本地年度数据缓存。判断优先级：**个人标记 > 公共放假补班记录 > 普通周末规则**。所有日期使用 `Asia/Shanghai`。

## NAS 部署

镜像：`ysjiang4869/chinese-holiday-api:2026.09.30`，linux/amd64。

```sh
mkdir -p /share/Public/container/chinese-holiday-api
# 首次运行，初始化此服务独立数据目录的非 root 容器权限
DOCKER_CONFIG=/share/homes/jysl/.docker docker run --rm --user 0 \
  -e DATA_DIR=/data -v /share/Public/container/chinese-holiday-api:/data \
  ysjiang4869/chinese-holiday-api:2026.09.30 initdata
DOCKER_CONFIG=/share/homes/jysl/.docker docker compose -f compose.yaml up -d
```

页面：`http://192.168.99.61:18300`。首次在内网创建管理员，密码至少 12 字符，然后在管理页面添加其他用户。每个用户可以创建多个命名 Token；完整值只在创建时显示一次，可分别撤销。Token 只能查询该用户的日历，不能修改标记或管理用户。

默认没有账号和默认密码。密码使用 PBKDF2-HMAC-SHA256（600,000 次），Token 只保存 SHA-256 摘要。登录会话有效期 24 小时，服务重启后需重新登录；密码、Token 和个人标记重启后保留。登录入口按直接连接 IP 每分钟最多 10 次请求（经 NPM 时可能共享这一限额）。

数据目录：

- `state.json`：用户、密码摘要、Token 摘要和各用户标记，属于私有数据。
- `years/YYYY.json`：公共年度缓存。镜像内置 2026 年数据，首次查询落盘。

管理员可在页面“更新公共日历”手动刷新年度数据；查询没有缓存的年份时尝试下载。没有后台 cron 或定时刷新。下载失败保留已有缓存；查询尚未公布/不可用年份返回 503，不会把缺失年份误判为普通工作日。更新日历不会覆盖个人标记。备份整个独立数据目录即可。

NAS 使用 `restart=no`，交由现有 `nas-staged-boot.sh` 动态发现并错峰启动。禁止改成 `always` 或 `unless-stopped`。

## NGINX Proxy Manager

添加 Proxy Host，将自己的域名转发到 `http://192.168.99.61:18300`，申请证书并开启 Force SSL。保留默认 Host 转发和 `X-Forwarded-Proto`，服务据此在 HTTPS 下设置 Secure 会话 Cookie。不需要 WebSocket。

首次初始化完成后再公开域名。页面、个人日历和 API 都需要鉴权；只有首页静态资源、初始化状态与 `/healthz` 无需登录。没有开放注册。浏览器写接口采用 SameSite=Strict Cookie、JSON 和自定义校验头，未开放 CORS。

## API

```sh
curl -H 'Authorization: Bearer <你的Token>' \
  'http://192.168.99.61:18300/api/holiday?date=2026-10-10'
```

省略 `date` 查询北京时间今天；也支持 `YYYY-MM` 完整月份、`YYYY` 完整年份。无效日期返回 400，不能回退成今天。

默认返回 JSON：

```json
{
  "code": 0,
  "message": "success",
  "timezone": "Asia/Shanghai",
  "user": "alice",
  "data": [{
    "date": "2026-10-10",
    "isRestDay": true,
    "type": 5,
    "kind": "personal_rest",
    "typeName": "个人休息",
    "name": "年假",
    "source": "manual",
    "baseType": 4,
    "baseName": "国庆节",
    "reason": "年假"
  }]
}
```

| type | kind | typeName | 是否休息 |
|---|---|---|---|
| 0 | workday | 普通工作日 | 否 |
| 1 | weekend | 普通周末 | 是 |
| 2 | holiday | 节假日放假 | 是 |
| 3 | makeup_before | 节前补班 | 否 |
| 4 | makeup_after | 节后补班 | 否 |
| 5 | personal_rest | 个人休息 | 是 |
| 6 | personal_work | 个人工作 | 否 |

`source` 为 `manual`（个人）、`official`（公共放假补班记录）或 `week_rule`（每周规则）。`baseType/baseName` 保留覆盖前安排，`reason` 为个人备注。`holiday` 指国务院放假安排中的休息日期，不用于计算法定加班工资或区分法定节日本日和连休日期。

兼容原接口的 `verbose=0`（单日 0/1）和 `verbose=1`（单日数字类型，新增 5/6）；`verbose=2` 返回 JSON。与原项目相比，默认改为 JSON，月/年查询返回所有日期，接口新增鉴权，无效日期不再默认为今天。

浏览器管理接口：`POST /api/override`（`isRestDay: null` 恢复自动）、`POST/DELETE /api/tokens`、管理员 `POST /api/users`、`POST /api/refresh`。全部需要登录会话和 `X-Calendar-Request: 1`，不接受查询 Token 写入。当前支持新增用户，不提供用户删除或密码重置页面。

Node-RED：HTTP request 节点 GET `/api/holiday`，请求头设置 `Authorization: Bearer …`，返回 JSON 对象；Switch 判断 `msg.payload.data[0].isRestDay`。非 200 或缺少有效结果走异常分支。

## 构建与验证

```sh
go test -race ./...
go vet ./...
docker build --platform linux/amd64 -t ysjiang4869/chinese-holiday-api:2026.09.30 .
```

标准 Dockerfile 使用 Go 多阶段构建，最终 scratch 镜像以 uid/gid 10001 运行，不含 shell。

本机基础镜像拉取遇到 502 时，可使用本地 Go 1.24+ 交叉编译：

```sh
mkdir -p release
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o release/calendar .
# macOS 的公共根证书集合，不包含私钥
cp /etc/ssl/cert.pem release/ca-certificates.crt
docker build --platform linux/amd64 -f Dockerfile.runtime -t ysjiang4869/chinese-holiday-api:2026.09.30 .
docker push ysjiang4869/chinese-holiday-api:2026.09.30
```
