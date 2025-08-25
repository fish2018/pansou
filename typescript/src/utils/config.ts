import { z } from 'zod';

// 配置验证模式
const ConfigSchema = z.object({
  serverUrl: z.string().url().default('http://localhost:8888'),
  requestTimeout: z.number().positive().default(30000),
  maxResults: z.number().positive().default(100),
  maxConcurrentRequests: z.number().positive().default(5),
  enableCache: z.boolean().default(false),
  defaultChannels: z.array(z.string()).default([]),
  defaultPlugins: z.array(z.string()).default([]),
  defaultCloudTypes: z.array(z.enum(['baidu', 'aliyun', 'quark', 'tianyi', 'uc', 'mobile', '115', 'pikpak', 'xunlei', '123', 'magnet', 'ed2k', 'others'])).default([]),
  logLevel: z.enum(['error', 'warn', 'info', 'debug']).default('info'),
  // 后端服务自动管理配置
  autoStartBackend: z.boolean().default(true),
  backendShutdownDelay: z.number().positive().default(5000),
  backendStartupTimeout: z.number().positive().default(30000),
  // 空闲超时配置（毫秒）
  idleTimeout: z.number().positive().default(300000), // 默认5分钟
  enableIdleShutdown: z.boolean().default(true),
  // 项目根目录路径
  projectRootPath: z.string().optional(),
  // Docker部署模式（当设置为true时，不会尝试启动本地进程）
  dockerMode: z.boolean().default(false)
});

export type Config = z.infer<typeof ConfigSchema>;

/**
 * 解析逗号分隔的字符串为数组
 */
function parseCommaSeparated(value: string | undefined): string[] {
  if (!value || value.trim() === '') {
    return [];
  }
  return value.split(',').map(item => item.trim()).filter(item => item.length > 0);
}

/**
 * 从环境变量加载配置
 */
export function loadConfig(): Config {
  const rawConfig = {
    serverUrl: process.env.PANSOU_SERVER_URL,
    requestTimeout: process.env.REQUEST_TIMEOUT ? parseInt(process.env.REQUEST_TIMEOUT) * 1000 : undefined,
    maxResults: process.env.MAX_RESULTS ? parseInt(process.env.MAX_RESULTS) : undefined,
    maxConcurrentRequests: process.env.MAX_CONCURRENT_REQUESTS ? parseInt(process.env.MAX_CONCURRENT_REQUESTS) : undefined,
    enableCache: process.env.ENABLE_CACHE === 'true',
    defaultChannels: parseCommaSeparated(process.env.DEFAULT_CHANNELS),
    defaultPlugins: parseCommaSeparated(process.env.DEFAULT_PLUGINS),
    defaultCloudTypes: parseCommaSeparated(process.env.DEFAULT_CLOUD_TYPES),
    logLevel: process.env.LOG_LEVEL as 'error' | 'warn' | 'info' | 'debug' | undefined,
    // 后端服务自动管理配置
    autoStartBackend: process.env.AUTO_START_BACKEND !== 'false', // 默认为true，除非明确设置为false
    backendShutdownDelay: process.env.BACKEND_SHUTDOWN_DELAY ? parseInt(process.env.BACKEND_SHUTDOWN_DELAY) : undefined,
    backendStartupTimeout: process.env.BACKEND_STARTUP_TIMEOUT ? parseInt(process.env.BACKEND_STARTUP_TIMEOUT) : undefined,
    // 空闲超时配置
    idleTimeout: process.env.IDLE_TIMEOUT ? parseInt(process.env.IDLE_TIMEOUT) : undefined,
    enableIdleShutdown: process.env.ENABLE_IDLE_SHUTDOWN !== 'false', // 默认为true，除非明确设置为false
    // 项目根目录路径
    projectRootPath: process.env.PROJECT_ROOT_PATH,
    // Docker部署模式
    dockerMode: process.env.DOCKER_MODE === 'true'
  };

  // 移除undefined值，让zod使用默认值
  const cleanConfig = Object.fromEntries(
    Object.entries(rawConfig).filter(([_, value]) => value !== undefined)
  );

  try {
    return ConfigSchema.parse(cleanConfig);
  } catch (error) {
    if (error instanceof z.ZodError) {
      console.error('配置验证失败:', error.errors);
      throw new Error(`配置验证失败: ${error.errors.map(e => `${e.path.join('.')}: ${e.message}`).join(', ')}`);
    }
    throw error;
  }
}

/**
 * 支持的网盘类型列表
 */
export const SUPPORTED_CLOUD_TYPES = [
  'baidu',    // 百度网盘
  'aliyun',   // 阿里云盘
  'quark',    // 夸克网盘
  'tianyi',   // 天翼云盘
  'uc',       // UC网盘
  'mobile',   // 移动云盘
  '115',      // 115网盘
  'pikpak',   // PikPak
  'xunlei',   // 迅雷网盘
  '123',      // 123网盘
  'magnet',   // 磁力链接
  'ed2k',     // 电驴链接
  'others'    // 其他
] as const;

export type CloudType = typeof SUPPORTED_CLOUD_TYPES[number];

/**
 * 支持的数据来源类型
 */
export const SOURCE_TYPES = ['all', 'tg', 'plugin'] as const;
export type SourceType = typeof SOURCE_TYPES[number];

/**
 * 支持的结果类型
 */
export const RESULT_TYPES = ['all', 'results', 'merge'] as const;
export type ResultType = typeof RESULT_TYPES[number];

/**
 * 验证网盘类型
 */
export function validateCloudTypes(cloudTypes: string[]): CloudType[] {
  const validTypes: CloudType[] = [];
  const invalidTypes: string[] = [];

  for (const type of cloudTypes) {
    if (SUPPORTED_CLOUD_TYPES.includes(type as CloudType)) {
      validTypes.push(type as CloudType);
    } else {
      invalidTypes.push(type);
    }
  }

  if (invalidTypes.length > 0) {
    throw new Error(`不支持的网盘类型: ${invalidTypes.join(', ')}。支持的类型: ${SUPPORTED_CLOUD_TYPES.join(', ')}`);
  }

  return validTypes;
}

/**
 * 验证数据来源类型
 */
export function validateSourceType(sourceType: string): SourceType {
  if (!SOURCE_TYPES.includes(sourceType as SourceType)) {
    throw new Error(`不支持的数据来源类型: ${sourceType}。支持的类型: ${SOURCE_TYPES.join(', ')}`);
  }
  return sourceType as SourceType;
}

/**
 * 验证结果类型
 */
export function validateResultType(resultType: string): ResultType {
  if (!RESULT_TYPES.includes(resultType as ResultType)) {
    throw new Error(`不支持的结果类型: ${resultType}。支持的类型: ${RESULT_TYPES.join(', ')}`);
  }
  return resultType as ResultType;
}