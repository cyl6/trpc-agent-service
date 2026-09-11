# IM 接入模拟控制台 — 后续开发详细指南

> 本文档基于当前已落盘的高保真视觉设计稿，详细规划从「静态设计稿」到「可部署生产级应用」的完整工程化路径。

---

## 一、项目现状评估

### 1.1 已交付产物

| 页面 | 文件 | 核心内容 | 交互状态 |
|------|------|----------|----------|
| 登录 | `pages/login.html` | 多租户卡片选择、管理令牌输入、保持登录 | 表单校验+提交模拟（硬编码 `local-admin`） |
| 控制台 | `pages/console.html` | 会话列表、消息流、筛选排序、发送消息 | 会话切换+筛选+发送+移动端抽屉 |
| 消息详情 | `pages/message-detail.html` | Trace时间线、JSON树、Tab切换、展开折叠 | Tab切换+JSON树展开折叠+全部展开 |
| 性能监控 | `pages/monitor.html` | 指标卡片、Chart.js图表、自动刷新 | 刷新按钮+自动刷新开关 |

### 1.2 技术栈现状

```
CSS引擎:  Tailwind CSS Browser 4.3.1 (CDN inline)
图标库:    Lucide Icons 1.8.0
图表库:    Chart.js 4.4.1
设计令牌:  源力设计系统 CSS变量 (inline <style id="theme-vars">)
页面间导航: 无路由，独立HTML文件
数据来源:  100% 硬编码静态数据
状态持久化: 未实现
```

### 1.3 已实现的交互逻辑清单

**login.html**
- 租户卡片单选切换（`selectCard()`）
- 管理令牌显示/隐藏切换
- 保持登录复选框toggle
- 令牌输入实时校验（`setSubmitEnabled()`）
- 模拟刷新租户列表（`setTimeout` 650ms）
- 表单提交校验（硬编码 `token !== 'local-admin'` 判断）
- 错误提示显示/隐藏

**console.html**
- 会话列表项点击切换active态
- 全部/未读筛选标签切换（基于 `data-unread` 属性）
- 单聊/群聊标签切换
- 排序下拉选择器（展开/折叠/选中）
- 移动端侧边栏抽屉（`toggleSidebar()`，`window.innerWidth <= 900` 断点）
- 发送消息（`sendMessage()`，创建DOM插入消息流）
- 回车发送 / Shift+回车换行
- textarea自动高度（max 120px）
- 消息流自动滚到底部

**message-detail.html**
- Tab切换（请求参数/响应数据/处理时间线）
- JSON树节点展开/折叠（`toggleNode()`）
- 全部展开/折叠按钮（深度>2的节点折叠）
- 时间线动画延迟（`animationDelay`）

**monitor.html**
- 自动刷新开关（15s间隔）
- 手动刷新按钮
- Chart.js图表渲染（静态数据）

### 1.4 关键差距总结

| 维度 | 现状 | 目标 |
|------|------|------|
| 数据层 | 硬编码HTML | 真实API + 本地缓存 |
| 路由 | 无 | SPA路由 |
| 状态管理 | 无 | 全局状态store |
| 认证 | 硬编码token校验 | JWT/OAuth + 拦截器 |
| 实时通信 | 无 | WebSocket/SSE |
| 持久化 | 无 | localStorage/IndexedDB |
| 性能 | DOM直接操作 | 虚拟滚动 + diff渲染 |
| 工程化 | 4个独立HTML | 组件化 + 构建工具链 |

---

## 二、架构改造方案

### 2.1 技术选型建议

```
框架:        React 18 + TypeScript 5.x
构建工具:     Vite 5.x
路由:        React Router 6
状态管理:     Zustand (轻量) 或 Redux Toolkit (中大型)
HTTP客户端:   Axios + 拦截器
实时通信:     native WebSocket + 重连封装
图表:        保留 Chart.js 或迁移 ECharts (更丰富的IM场景图表)
CSS方案:     保留 Tailwind CSS (npm安装，非CDN) + CSS变量
测试:        Vitest + React Testing Library
```

### 2.2 目录结构规划

```
im-console/
├── src/
│   ├── api/                    # API层
│   │   ├── client.ts           # Axios实例 + 拦截器
│   │   ├── auth.ts             # 认证接口
│   │   ├── conversations.ts    # 会话接口
│   │   ├── messages.ts         # 消息接口
│   │   ├── trace.ts            # Trace接口
│   │   └── metrics.ts          # 性能指标接口
│   ├── components/             # 通用组件
│   │   ├── layout/
│   │   │   ├── AppShell.tsx    # 应用外壳（顶栏+侧栏+内容区）
│   │   │   ├── Sidebar.tsx
│   │   │   └── TopBar.tsx
│   │   ├── ui/                 # 基础UI组件
│   │   │   ├── Button.tsx
│   │   │   ├── Input.tsx
│   │   │   ├── Select.tsx
│   │   │   ├── Checkbox.tsx
│   │   │   ├── Tabs.tsx
│   │   │   ├── Modal.tsx
│   │   │   ├── Toast.tsx
│   │   │   └── Badge.tsx
│   │   ├── chat/
│   │   │   ├── MessageBubble.tsx
│   │   │   ├── MessageList.tsx
│   │   │   ├── MessageInput.tsx
│   │   │   └── TypingIndicator.tsx
│   │   ├── trace/
│   │   │   ├── TraceTimeline.tsx
│   │   │   ├── JsonTree.tsx
│   │   │   └── TraceNode.tsx
│   │   └── charts/
│   │       ├── MetricCard.tsx
│   │       ├── TokenChart.tsx
│   │       └── CacheHitChart.tsx
│   ├── pages/                  # 页面组件
│   │   ├── LoginPage.tsx
│   │   ├── ConsolePage.tsx
│   │   ├── MessageDetailPage.tsx
│   │   └── MonitorPage.tsx
│   ├── stores/                 # 状态管理
│   │   ├── authStore.ts
│   │   ├── conversationStore.ts
│   │   ├── messageStore.ts
│   │   └── metricStore.ts
│   ├── hooks/                  # 自定义Hooks
│   │   ├── useWebSocket.ts
│   │   ├── useLocalStorage.ts
│   │   ├── useVirtualScroll.ts
│   │   └── useInfiniteScroll.ts
│   ├── types/                  # TypeScript类型定义
│   │   ├── api.ts
│   │   ├── models.ts
│   │   └── trace.ts
│   ├── utils/                  # 工具函数
│   │   ├── request.ts
│   │   ├── storage.ts
│   │   ├── format.ts
│   │   └── error.ts
│   ├── styles/
│   │   ├── theme.css          # 源力设计系统CSS变量（从现有HTML提取）
│   │   └── global.css
│   ├── router.tsx
│   ├── App.tsx
│   └── main.tsx
├── public/
│   └── assets/
│       └── icons/             # 源力图标资源（已落盘597个）
├── .env                       # 环境变量
├── vite.config.ts
├── tsconfig.json
└── package.json
```

### 2.3 路由设计

```typescript
// src/router.tsx
const routes = [
  {
    path: '/login',
    element: <LoginPage />,
    public: true,  // 不需要认证
  },
  {
    path: '/',
    element: <AppShell />,  // 布局壳，包含认证守卫
    children: [
      { index: true, element: <Navigate to="/console" /> },
      { path: 'console', element: <ConsolePage /> },
      { path: 'message/:messageId', element: <MessageDetailPage /> },
      { path: 'monitor', element: <MonitorPage /> },
    ],
  },
];

// 认证守卫
function AppShell() {
  const { isAuthenticated } = useAuthStore();
  if (!isAuthenticated) return <Navigate to="/login" />;
  return <Outlet />;  // 渲染子路由
}
```

---

## 三、API接口对接方案

### 3.1 认证模块

```typescript
// src/api/auth.ts

interface LoginRequest {
  tenantId: string;
  adminToken: string;
  mockUserId?: string;
  keepLogin?: boolean;
}

interface LoginResponse {
  accessToken: string;
  refreshToken?: string;
  expiresAt: number;         // unix timestamp
  tenant: {
    id: string;
    name: string;
    avatar?: string;
  };
  user: {
    id: string;
    name: string;
  };
}

// 登录
export async function login(req: LoginRequest): Promise<LoginResponse> {
  const { data } = await apiClient.post('/auth/login', req);
  return data;
}

// 刷新token
export async function refreshToken(refreshToken: string): Promise<LoginResponse> {
  const { data } = await apiClient.post('/auth/refresh', { refreshToken });
  return data;
}

// 获取租户列表
export async function getTenants(token: string): Promise<Tenant[]> {
  const { data } = await apiClient.get('/tenants', {
    headers: { 'X-Admin-Token': token },
  });
  return data;
}

// 登出
export async function logout(): Promise<void> {
  await apiClient.post('/auth/logout');
}
```

### 3.2 会话模块

```typescript
// src/api/conversations.ts

interface Conversation {
  id: string;
  name: string;
  type: 'single' | 'group';
  lastMessage: {
    content: string;
    timestamp: number;
    senderId: string;
  };
  unreadCount: number;
  lastActiveAt: number;
}

interface ConversationListParams {
  page?: number;
  pageSize?: number;
  keyword?: string;          // 关键词搜索
  filter?: 'all' | 'unread'; // 条件筛选
  scope?: 'single' | 'group'; // 单聊/群聊
  sort?: 'time' | 'name' | 'unread'; // 排序
}

// 获取会话列表
export async function getConversations(
  params: ConversationListParams
): Promise<{ items: Conversation[]; total: number }> {
  const { data } = await apiClient.get('/conversations', { params });
  return data;
}

// 标记已读
export async function markRead(conversationId: string): Promise<void> {
  await apiClient.post(`/conversations/${conversationId}/read`);
}
```

### 3.3 消息模块

```typescript
// src/api/messages.ts

interface Message {
  id: string;
  conversationId: string;
  senderId: string;
  senderName: string;
  senderType: 'user' | 'agent' | 'system';
  content: string;
  timestamp: number;
  status: 'sending' | 'sent' | 'delivered' | 'failed';
  traceId?: string;          // 关联Trace
  tokenUsage?: {
    input: number;
    output: number;
    total: number;
  };
}

interface MessageListParams {
  conversationId: string;
  beforeId?: string;         // 游标分页：加载此ID之前的消息
  limit?: number;            // 默认50
}

// 获取消息历史（游标分页，支持向上加载）
export async function getMessages(
  params: MessageListParams
): Promise<{ items: Message[]; hasMore: boolean }> {
  const { data } = await apiClient.get('/messages', { params });
  return data;
}

// 发送消息
export async function sendMessage(
  conversationId: string,
  content: string
): Promise<Message> {
  const { data } = await apiClient.post('/messages/send', {
    conversationId,
    content,
  });
  return data;
}

// 重发失败消息
export async function retryMessage(messageId: string): Promise<Message> {
  const { data } = await apiClient.post(`/messages/${messageId}/retry`);
  return data;
}
```

### 3.4 Trace模块

```typescript
// src/api/trace.ts

interface TraceNode {
  id: string;
  name: string;              // 节点名称：如"接收请求"、"LLM调用"
  type: 'request' | 'llm_call' | 'cache_lookup' | 'response' | 'tool_call';
  status: 'success' | 'error' | 'timeout';
  startTime: number;         // ms timestamp
  endTime: number;
  duration: number;          // ms
  children?: TraceNode[];
  metadata?: {
    requestParams?: Record<string, unknown>;
    responseData?: Record<string, unknown>;
    tokenUsage?: { input: number; output: number };
    cacheKey?: string;
    cacheHit?: boolean;
  };
}

// 获取消息Trace
export async function getMessageTrace(messageId: string): Promise<{
  traceId: string;
  rootNodes: TraceNode[];
  totalDuration: number;
  tokenUsage: { input: number; output: number; total: number };
}> {
  const { data } = await apiClient.get(`/trace/${messageId}`);
  return data;
}
```

### 3.5 性能指标模块

```typescript
// src/api/metrics.ts

interface MetricsSummary {
  totalMessages: number;
  sentCount: number;
  receivedCount: number;
  tokenUsage: {
    input: number;
    output: number;
    total: number;
  };
  estimatedCost: number;
  cacheHitRate: number;       // 0-1
  avgResponseTime: number;   // ms
  activeConversations: number;
}

interface MetricsTimeSeries {
  timestamps: number[];
  messageCounts: number[];
  tokenUsages: number[];
  cacheHitRates: number[];
}

// 获取指标概览
export async function getMetricsSummary(
  range?: '1h' | '24h' | '7d'
): Promise<MetricsSummary> {
  const { data } = await apiClient.get('/metrics/summary', { params: { range } });
  return data;
}

// 获取时序数据
export async function getMetricsTimeSeries(
  range: '1h' | '24h' | '7d',
  interval: '1m' | '5m' | '1h'
): Promise<MetricsTimeSeries> {
  const { data } = await apiClient.get('/metrics/timeseries', {
    params: { range, interval },
  });
  return data;
}
```

---

## 四、状态管理设计

### 4.1 认证状态

```typescript
// src/stores/authStore.ts
import { create } from 'zustand';

interface AuthState {
  isAuthenticated: boolean;
  accessToken: string | null;
  refreshToken: string | null;
  expiresAt: number | null;
  tenant: { id: string; name: string } | null;
  user: { id: string; name: string } | null;
  preferences: {
    keepLogin: boolean;
    lastTenantId: string | null;
    theme: 'light' | 'dark';
  };
  // actions
  login: (response: LoginResponse) => void;
  logout: () => void;
  updatePreferences: (prefs: Partial<AuthState['preferences']>) => void;
  refreshTokenIfNeeded: () => Promise<void>;
}

export const useAuthStore = create<AuthState>((set, get) => ({
  // 初始值从localStorage恢复
  isAuthenticated: !!localStorage.getItem('im_access_token'),
  accessToken: localStorage.getItem('im_access_token'),
  // ...

  login: (response) => {
    const { keepLogin } = get().preferences;
    // 根据keepLogin决定存sessionStorage还是localStorage
    const storage = keepLogin ? localStorage : sessionStorage;
    storage.setItem('im_access_token', response.accessToken);
    if (response.refreshToken) {
      storage.setItem('im_refresh_token', response.refreshToken);
    }
    set({
      isAuthenticated: true,
      accessToken: response.accessToken,
      refreshToken: response.refreshToken,
      expiresAt: response.expiresAt,
      tenant: response.tenant,
      user: response.user,
    });
  },

  logout: () => {
    localStorage.removeItem('im_access_token');
    localStorage.removeItem('im_refresh_token');
    sessionStorage.removeItem('im_access_token');
    sessionStorage.removeItem('im_refresh_token');
    set({
      isAuthenticated: false,
      accessToken: null,
      refreshToken: null,
      tenant: null,
      user: null,
    });
  },
}));
```

### 4.2 会话状态

```typescript
// src/stores/conversationStore.ts
interface ConversationState {
  conversations: Conversation[];
  activeConversationId: string | null;
  loading: boolean;
  error: string | null;
  searchKeyword: string;
  filter: 'all' | 'unread';
  scope: 'all' | 'single' | 'group';
  sortBy: 'time' | 'name' | 'unread';

  // actions
  fetchConversations: () => Promise<void>;
  setActiveConversation: (id: string) => void;
  setSearchKeyword: (keyword: string) => void;
  setFilter: (filter: 'all' | 'unread') => void;
  setSortBy: (sort: 'time' | 'name' | 'unread') => void;
  updateLastMessage: (conversationId: string, message: Message) => void;
  incrementUnread: (conversationId: string) => void;
  clearUnread: (conversationId: string) => void;
}
```

### 4.3 消息状态

```typescript
// src/stores/messageStore.ts
interface MessageState {
  messagesByConversation: Map<string, Message[]>;
  loading: boolean;
  hasMore: boolean;
  sending: boolean;
  error: string | null;

  // actions
  fetchMessages: (conversationId: string, beforeId?: string) => Promise<void>;
  sendMessage: (conversationId: string, content: string) => Promise<void>;
  receiveMessage: (message: Message) => void;  // WebSocket推送
  retryMessage: (messageId: string) => Promise<void>;
  updateMessageStatus: (messageId: string, status: Message['status']) => void;
}
```

---

## 五、实时通信方案

### 5.1 WebSocket封装

```typescript
// src/hooks/useWebSocket.ts
import { useEffect, useRef, useCallback } from 'react';

interface WSOptions {
  url: string;
  token: string;
  onMessage: (data: WSMessage) => void;
  onConnect?: () => void;
  onDisconnect?: () => void;
  reconnectInterval?: number;    // 默认3000ms
  maxReconnectAttempts?: number; // 默认10
}

type WSMessage =
  | { type: 'message_new'; data: Message }
  | { type: 'message_status'; data: { messageId: string; status: string } }
  | { type: 'typing'; data: { conversationId: string; userId: string } }
  | { type: 'conversation_update'; data: Conversation }
  | { type: 'metrics_update'; data: Partial<MetricsSummary> };

export function useWebSocket(options: WSOptions) {
  const wsRef = useRef<WebSocket | null>(null);
  const reconnectCount = useRef(0);
  const reconnectTimer = useRef<ReturnType<typeof setTimeout>>();

  const connect = useCallback(() => {
    const ws = new WebSocket(`${options.url}?token=${options.token}`);

    ws.onopen = () => {
      reconnectCount.current = 0;
      options.onConnect?.();
    };

    ws.onmessage = (event) => {
      try {
        const data: WSMessage = JSON.parse(event.data);
        options.onMessage(data);
      } catch (e) {
        console.error('WebSocket消息解析失败', e);
      }
    };

    ws.onclose = () => {
      options.onDisconnect?.();
      // 自动重连（指数退避）
      if (reconnectCount.current < (options.maxReconnectAttempts ?? 10)) {
        const delay = (options.reconnectInterval ?? 3000) *
          Math.pow(1.5, reconnectCount.current);
        reconnectTimer.current = setTimeout(connect, delay);
        reconnectCount.current++;
      }
    };

    wsRef.current = ws;
  }, [options.url, options.token]);

  useEffect(() => {
    connect();
    return () => {
      clearTimeout(reconnectTimer.current);
      wsRef.current?.close();
    };
  }, [connect]);

  // 心跳保活
  useEffect(() => {
    const heartbeat = setInterval(() => {
      if (wsRef.current?.readyState === WebSocket.OPEN) {
        wsRef.current.send(JSON.stringify({ type: 'ping' }));
      }
    }, 30000);
    return () => clearInterval(heartbeat);
  }, []);

  return { send: (data: unknown) => wsRef.current?.send(JSON.stringify(data)) };
}
```

### 5.2 SSE备选方案（仅接收场景）

```typescript
// src/hooks/useSSE.ts
// 当只需要服务端推送、不需要客户端实时上报时使用SSE更轻量
export function useSSE(url: string, token: string, onMessage: (data: unknown) => void) {
  useEffect(() => {
    const es = new EventSource(`${url}?token=${token}`);

    es.onmessage = (event) => {
      try {
        const data = JSON.parse(event.data);
        onMessage(data);
      } catch (e) {
        console.error('SSE消息解析失败', e);
      }
    };

    es.onerror = () => {
      // 浏览器会自动重连SSE
      console.warn('SSE连接断开，浏览器自动重连中');
    };

    return () => es.close();
  }, [url, token, onMessage]);
}
```

---

## 六、性能优化方案

### 6.1 虚拟滚动（大量消息场景）

```typescript
// src/hooks/useVirtualScroll.ts
// 当单会话消息超过200条时启用虚拟滚动，只渲染可视区域+缓冲区
import { useRef, useState, useEffect, useCallback } from 'react';

interface VirtualScrollOptions {
  itemCount: number;
  itemHeight: number;       // 预估每条消息高度
  overscan: number;        // 上下缓冲条数，默认5
  containerHeight: number;
}

export function useVirtualScroll({
  itemCount,
  itemHeight,
  overscan = 5,
  containerHeight,
}: VirtualScrollOptions) {
  const [scrollTop, setScrollTop] = useState(0);

  const startIndex = Math.max(0, Math.floor(scrollTop / itemHeight) - overscan);
  const endIndex = Math.min(
    itemCount - 1,
    Math.ceil((scrollTop + containerHeight) / itemHeight) + overscan
  );

  const visibleItems = Array.from(
    { length: endIndex - startIndex + 1 },
    (_, i) => startIndex + i
  );

  const totalHeight = itemCount * itemHeight;
  const offsetY = startIndex * itemHeight;

  const onScroll = useCallback((e: React.UIEvent<HTMLDivElement>) => {
    setScrollTop(e.currentTarget.scrollTop);
  }, []);

  return { visibleItems, totalHeight, offsetY, onScroll, startIndex, endIndex };
}
```

> 注意：消息高度不固定时，需要用动态高度虚拟滚动（维护测量缓存），可使用 `react-window` 或 `@tanstack/react-virtual` 库替代手写。

### 6.2 消息渲染优化

```typescript
// React.memo 避免不必要重渲染
const MessageBubble = React.memo(function MessageBubble({
  message,
  onRetry,
}: {
  message: Message;
  onRetry?: (id: string) => void;
}) {
  // 只在message引用变化时重渲染
  return <div className="im-msg">...</div>;
}, (prev, next) => {
  // 自定义比较：id、status、content变化时才重渲染
  return prev.message.id === next.message.id &&
         prev.message.status === next.message.status &&
         prev.message.content === next.message.content;
});

// 使用useCallback稳定事件引用
const handleMessageRetry = useCallback((id: string) => {
  retryMessage(id);
}, []);
```

### 6.3 数据缓存策略

```typescript
// 使用React Query或Zustand middleware实现
// 会话列表：缓存5分钟，过期后台静默刷新
// 消息历史：永久缓存（IndexedDB），仅增量拉取新消息
// Trace数据：缓存1小时（Trace不变）
// 性能指标：不缓存（实时性要求高）

// 会话列表缓存
const { data: conversations } = useQuery({
  queryKey: ['conversations', { keyword, filter, scope, sortBy }],
  queryFn: () => getConversations({ keyword, filter, scope, sort: sortBy }),
  staleTime: 5 * 60 * 1000,  // 5分钟
  refetchOnWindowFocus: true, // 窗口聚焦时后台刷新
});

// 消息历史（游标分页 + IndexedDB离线缓存）
const { data, fetchNextPage } = useInfiniteQuery({
  queryKey: ['messages', conversationId],
  queryFn: ({ pageParam }) => getMessages({
    conversationId,
    beforeId: pageParam,
  }),
  getNextPageParam: (lastPage) =>
    lastPage.hasMore ? lastPage.items[0]?.id : undefined,
  staleTime: Infinity,  // 消息不变，永久fresh
});
```

---

## 七、错误处理方案

### 7.1 HTTP拦截器

```typescript
// src/api/client.ts
import axios from 'axios';

export const apiClient = axios.create({
  baseURL: import.meta.env.VITE_API_BASE_URL,
  timeout: 15000,
});

// 请求拦截：附加token
apiClient.interceptors.request.use((config) => {
  const token = useAuthStore.getState().accessToken;
  if (token) {
    config.headers.Authorization = `Bearer ${token}`;
  }
  return config;
});

// 响应拦截：统一错误处理
apiClient.interceptors.response.use(
  (response) => response,
  async (error) => {
    if (!error.response) {
      // 网络错误
      toast.error('网络连接异常，请检查网络后重试');
      return Promise.reject(error);
    }

    const { status, data } = error.response;

    switch (status) {
      case 401:
        // token过期，尝试刷新
        try {
          await useAuthStore.getState().refreshTokenIfNeeded();
          // 重新发送原请求
          return apiClient(error.config);
        } catch {
          // 刷新失败，跳转登录
          useAuthStore.getState().logout();
          toast.info('登录已过期，请重新登录');
          window.location.href = '/login';
        }
        break;

      case 403:
        toast.error('没有权限执行此操作');
        break;

      case 429:
        toast.warning('操作过于频繁，请稍后再试');
        break;

      case 500:
        toast.error('服务暂时不可用，请稍后重试');
        break;

      default:
        toast.error(data?.message || '请求失败，请重试');
    }

    return Promise.reject(error);
  }
);
```

### 7.2 友好提示组件

```typescript
// Toast组件设计要求（对应现有视觉风格）：
// - 使用 --color-warning / --color-danger 的低饱和变体
// - 背景 --color-surface-container (浅色)
// - 圆角 --radius-md
// - 阴影柔和 --shadow-sm
// - 自动消失 3-5s，可手动关闭
// - 不使用强警示红色满屏，用柔和的左侧色条标识级别

// src/components/ui/Toast.tsx
const toastStyles = {
  success: {
    borderColor: 'var(--color-success)',
    bgColor: 'var(--color-success-bg)',
    icon: 'check-circle',
  },
  warning: {
    borderColor: 'var(--color-warning)',
    bgColor: 'var(--color-warning-bg)',
    icon: 'alert-triangle',
  },
  error: {
    borderColor: 'var(--color-danger)',
    bgColor: 'var(--color-danger-bg)',
    icon: 'alert-circle',
  },
  info: {
    borderColor: 'var(--color-primary)',
    bgColor: 'var(--color-primary-bg)',
    icon: 'info',
  },
};
```

### 7.3 空状态与加载态

```typescript
// 每个数据列表都应有三种状态处理：
// 1. loading: 骨架屏（柔和灰色占位块，不使用全屏spinner）
// 2. empty: 空状态插画+引导文案
// 3. error: 错误重试按钮+友好提示

// 骨架屏示例
function ConversationSkeleton() {
  return (
    <div className="animate-pulse space-y-3 p-3">
      {[1, 2, 3, 4, 5].map((i) => (
        <div key={i} className="flex items-center gap-3">
          <div className="w-10 h-10 rounded-full bg-[var(--color-surface-container-high)]" />
          <div className="flex-1 space-y-2">
            <div className="h-3 w-1/2 rounded bg-[var(--color-surface-container-high)]" />
            <div className="h-2 w-3/4 rounded bg-[var(--color-surface-container)]" />
          </div>
        </div>
      ))}
    </div>
  );
}
```

---

## 八、可扩展性设计

### 8.1 插槽机制

```typescript
// 预留IM业务模块扩展插槽
// 通过插件化注册实现，新模块只需注册组件和路由

// src/plugins/registry.ts
interface IMPlugin {
  name: string;
  routes?: Array<{ path: string; element: React.ReactNode }>;
  sidebarItem?: { icon: string; label: string; path: string };
  messageAction?: (message: Message) => React.ReactNode; // 消息操作扩展
  store?: unknown;  // 插件自带状态
}

const pluginRegistry: IMPlugin[] = [];

export function registerPlugin(plugin: IMPlugin) {
  pluginRegistry.push(plugin);
}

// 使用示例：未来添加"智能客服"模块
// registerPlugin({
//   name: 'smart-agent',
//   routes: [{ path: 'agent', element: <AgentPage /> }],
//   sidebarItem: { icon: 'ai', label: '智能客服', path: '/agent' },
// });
```

### 8.2 消息类型扩展

```typescript
// src/types/models.ts
// 消息内容支持多类型，方便后续扩展富文本/卡片/文件等

interface BaseMessage {
  id: string;
  conversationId: string;
  senderId: string;
  timestamp: number;
  status: 'sending' | 'sent' | 'delivered' | 'failed';
}

interface TextMessage extends BaseMessage {
  type: 'text';
  content: string;
}

interface CardMessage extends BaseMessage {
  type: 'card';
  card: {
    title: string;
    description: string;
    actions: Array<{ label: string; action: string }>;
  };
}

interface FileMessage extends BaseMessage {
  type: 'file';
  file: { name: string; url: string; size: number; mimeType: string };
}

type Message = TextMessage | CardMessage | FileMessage;

// 渲染器注册表
const messageRenderers: Record<string, React.FC<{ message: Message }>> = {
  text: TextMessageRenderer,
  card: CardMessageRenderer,
  file: FileMessageRenderer,
};
// 新增类型只需注册renderer
```

---

## 九、开发阶段规划

### 阶段一：工程化骨架（1-2周）

**目标**：将4个独立HTML迁移为Vite + React + TypeScript项目

- 初始化Vite项目，配置Tailwind CSS（npm版替代CDN）
- 从现有HTML提取源力设计系统CSS变量到 `theme.css`
- 拆分4个页面为React组件，保持现有视觉完全一致
- 配置React Router，实现页面间导航
- 搭建Axios客户端 + 拦截器骨架
- 配置ESLint + Prettier + TypeScript严格模式

**验收标准**：4个页面在React项目中视觉与现有HTML完全一致，路由可正常跳转。

### 阶段二：认证与数据层（1-2周）

**目标**：接入真实后端认证和基础数据接口

- 实现登录页面与认证API对接（替换硬编码 `local-admin`）
- 实现token存储与自动刷新机制
- 实现认证守卫路由
- 对接租户列表API
- 对接会话列表API（含搜索/筛选/排序）
- 实现 `localStorage` 持久化登录态和用户偏好

**验收标准**：可使用真实租户账号登录，会话列表从后端加载，刷新页面保持登录态。

### 阶段三：消息系统（2-3周）

**目标**：实现完整的消息收发和实时通信

- 对接消息历史API（游标分页）
- 实现WebSocket连接（含重连、心跳）
- 实现消息实时接收与渲染
- 实现消息发送（含发送中/成功/失败状态流转）
- 实现消息重发
- 实现未读消息计数与标记已读
- 实现虚拟滚动（消息量>200条时）

**验收标准**：可实时收发消息，大量消息滚动流畅，断网重连后消息不丢失。

### 阶段四：Trace与监控（1-2周）

**目标**：完善消息详情和性能监控模块

- 对接Trace API，动态渲染Trace树
- 实现Trace节点展开/折叠（递归组件）
- 对接性能指标API
- 实现Chart.js图表动态数据渲染
- 实现自动刷新（15s轮询或WebSocket推送）

**验收标准**：点击消息可查看完整Trace链路，性能监控图表实时更新。

### 阶段五：健壮性与优化（1周）

**目标**：异常处理、性能优化、用户体验打磨

- 完善错误处理（网络异常、接口超时、服务端错误）
- 实现Toast通知系统
- 实现骨架屏和空状态
- 实现消息搜索全文检索
- 完成响应式适配测试（移动端/平板/桌面）
- 编写单元测试覆盖核心逻辑

**验收标准**：各种异常场景有友好提示，核心逻辑有测试覆盖，多设备体验一致。

---

## 十、设计令牌迁移清单

以下CSS变量已内联在现有HTML的 `<style id="theme-vars">` 中，迁移到React项目时提取到 `src/styles/theme.css`：

```css
:root {
  /* 主色 — 源力蓝 */
  --primary-1: #F3F7FF;  --primary-6: #1664FF;
  --primary-2: #EBF1FF;  --primary-7: #1759DD;
  --primary-3: #97BCFF;  --primary-8: #114AB9;
  --primary-4: #6E9FFF;
  --primary-5: #387BFF;

  /* 功能色 */
  --teal-6: #24758E;     --violet-6: #6E4C9F;
  --teal-5: #3888A3;     --violet-5: #876EB8;

  /* 语义别名 */
  --color-primary: var(--primary-6);
  --color-primary-hover: var(--primary-5);
  --color-danger: var(--danger-6);
  --color-success: var(--success-6);
  --color-warning: var(--warning-6);

  /* 文字色阶 */
  --color-text-1: #0C0D0E;  --color-text-3: #737A87;
  --color-text-2: #42464E;  --color-text-4: #C7CCD6;

  /* 背景层次 */
  --color-surface: #FFFFFF;
  --color-surface-container: #F7F9FB;
  --color-surface-container-high: #F1F4F8;

  /* 完整色阶见现有HTML <style id="theme-vars"> */
}
```

> 迁移原则：所有颜色必须使用CSS变量，严禁硬编码hex值。新增组件样式同样遵守此规则。

---

## 十一、关键代码迁移映射

| 现有HTML中的JS | 迁移后React对应 | 说明 |
|----------------|-----------------|------|
| `selectCard()` | `LoginPage` 组件内 `useState` | 租户选择状态 |
| `setSubmitEnabled()` | 表单校验逻辑 + `disabled` prop | 替换手动class toggle |
| `sendMessage()` | `messageStore.sendMessage()` | 替换DOM操作为状态驱动 |
| `convItems.forEach(click)` | `conversationStore.setActive` | 替换DOM class为状态绑定 |
| `toggleSidebar()` | 响应式CSS + `useState` | Tailwind `md:` 断点 |
| `toggleNode()` | `JsonTree` 递归组件 | 每个节点独立展开状态 |
| `autoRefresh` | `setInterval` + `useEffect` cleanup | 注意组件卸载时清除定时器 |
| 硬编码token校验 | `authStore.login()` + API | 替换 `token !== 'local-admin'` |
| `data-unread` 属性 | `conversation.unreadCount` | 状态驱动渲染 |

---

## 附录：环境变量配置

```bash
# .env
VITE_API_BASE_URL=http://localhost:8080/api
VITE_WS_URL=ws://localhost:8080/ws
VITE_SENTRY_DSN=               # 可选：错误监控
VITE_ENABLE_MOCK=false          # 开发阶段可开启mock
```

```typescript
// src/api/client.ts
const apiClient = axios.create({
  baseURL: import.meta.env.VITE_API_BASE_URL,
  // ...
});
```
