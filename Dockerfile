# 构建阶段
# 使用 --platform=$BUILDPLATFORM 确保构建器始终在运行 Actions 的机器的原生架构上运行 (通常是 linux/amd64)
# $BUILDPLATFORM 是 buildx 自动提供的变量
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

# 安装构建依赖
RUN apk add --no-cache git ca-certificates tzdata

# 设置工作目录
WORKDIR /app

# 复制依赖文件
COPY go.mod go.sum ./

# 下载依赖
RUN go mod download

# 复制源代码
COPY . .

# 构建参数
ARG VERSION=dev
ARG BUILD_DATE=unknown
ARG VCS_REF=unknown

# 这是 buildx 自动传入的目标平台架构参数，例如 amd64, arm64
ARG TARGETARCH

# 构建应用
# Go 语言原生支持交叉编译，这里会根据传入的 TARGETARCH 编译出对应平台的可执行文件
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-s -w -extldflags '-static'" -o pansou .

# 运行阶段
# 这一阶段会根据 buildx 的 --platform 参数选择正确的基础镜像 (例如 linux/arm64 会拉取 arm64/alpine)
FROM alpine:3.19

# 添加运行时依赖
RUN apk add --no-cache ca-certificates tzdata

# 创建缓存目录
RUN mkdir -p /app/cache

# 从构建阶段复制可执行文件
# buildx 会智能地从对应平台的 builder 中复制正确的可执行文件
COPY --from=builder /app/pansou /app/pansou

# 设置工作目录
WORKDIR /app

# 暴露端口
EXPOSE 8888

# 设置环境变量
# ENABLED_PLUGINS: 必须指定启用的插件，多个插件用逗号分隔
# AUTH_ENABLED: 认证功能默认关闭，可通过环境变量启用
ENV CACHE_PATH=/app/cache \
    CACHE_ENABLED=true \
    TZ=Asia/Shanghai \
    ASYNC_PLUGIN_ENABLED=true \
    ASYNC_RESPONSE_TIMEOUT=4 \
    ASYNC_MAX_BACKGROUND_WORKERS=20 \
    ASYNC_MAX_BACKGROUND_TASKS=100 \
    ASYNC_CACHE_TTL_HOURS=1 \
    CHANNELS=tgsearchers6,Aliyun_4K_Movies,bdbdndn11,yunpanx,bsbdbfjfjff,yp123pan,sbsbsnsqq,yunpanxunlei,tianyifc,BaiduCloudDisk,txtyzy,peccxinpd,gotopan,PanjClub,kkxlzy,baicaoZY,MCPH01,MCPH02,MCPH03,bdwpzhpd,ysxb48,jdjdn1111,yggpan,MCPH086,zaihuayun,Q66Share,ucwpzy,shareAliyun,alyp_1,dianyingshare,Quark_Movies,XiangxiuNBB,ydypzyfx,ucquark,xx123pan,yingshifenxiang123,zyfb123,tyypzhpd,tianyirigeng,cloudtianyi,hdhhd21,Lsp115,oneonefivewpfx,qixingzhenren,taoxgzy,Channel_Shares_115,tyysypzypd,vip115hot,wp123zy,yunpan139,yunpan189,yunpanuc,yydf_hzl,leoziyuan,pikpakpan,Q_dongman,yoyokuakeduanju,TG654TG,WFYSFX02,QukanMovie,yeqingjie_GJG666,movielover8888_film3,Baidu_netdisk,D_wusun,FLMdongtianfudi,KaiPanshare,QQZYDAPP,rjyxfx,PikPak_Share_Channel,btzhi,newproductsourcing,cctv1211,duan_ju,QuarkFree,yunpanNB,kkdj001,xxzlzn,pxyunpanxunlei,jxwpzy,kuakedongman,liangxingzhinan,xiangnikanj,solidsexydoll,guoman4K,zdqxm,kduanju,cilidianying,CBduanju,SharePanFilms,dzsgx,BooksRealm,Oscar_4Kmovies,douerpan,baidu_yppan,Q_jilupian,Netdisk_Movies,yunpanquark,ammmziyuan,ciliziyuanku,cili8888,jzmm_123pan,wpan8,mqte5,regengguangya,yunpanguangya,regeng115,regeng123,yoyokuakeduanjujiaoliuqun,yy80986098,pan_guangya,guangyapan_episode,AV688 \
    ENABLED_PLUGINS=dyyjpro,duoduo,feikuai,gaoqing888,gying,haitunsou,hunhepan,ikantv,jutoushe,kkv,dy4k,libvio,lingjisp,lou1,melost,miosou,nyaa,ouge,panlian,qqpd,quark4k,quarksoo,quarktv,qupanshe,sousou,thepiratebay,ting77,wanou,weibo,xb6v,xiaokupan,xiaozhang,xiaoyu,yingso,yunso,zlxapp,zxzj \
    AUTH_ENABLED=false \
    AUTH_TOKEN_EXPIRY=24

# 构建参数
ARG VERSION=dev
ARG BUILD_DATE=unknown
ARG VCS_REF=unknown

# 添加镜像标签
LABEL org.opencontainers.image.title="PanSou" \
      org.opencontainers.image.description="高性能网盘资源搜索API服务" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.url="https://github.com/fish2018/pansou" \
      org.opencontainers.image.source="https://github.com/fish2018/pansou" \
      maintainer="fish2018"

# 运行应用
CMD ["/app/pansou"]
