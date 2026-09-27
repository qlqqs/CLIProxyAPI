# 配置热加载与对象存储镜像

## 1. 适用范围

修改 `internal/watcher/` 的配置/凭据热加载或 `ObjectTokenStore` 的对象镜像同步时读取。目标是运行时状态与实际应用的快照一致，不能因文件替换漏更新，也不能恢复远端已删除的凭据。

## 2. 接口与状态

- `(*Watcher).Start(context.Context)`、`ReloadConfigIfChanged()`、`Stop()`。
- `reloadConfigWithForce(bool) bool` 串行化解析与应用；`lastConfigHash` 只代表已应用的解析结果。
- `switchAuthDirWatch(oldDir, newDir string) error` 与凭据快照切换共用 `authRescanMu`。
- `(*ObjectTokenStore).syncAuthFromBucket(context.Context) error` 在 store 锁内完成镜像对账；`syncedAuthRemovals` 抑制镜像删除的 watcher 回传。

## 3. 行为合同

- 配置 watch 注册在父目录，仅按目标文件名接收事件；重复原子 rename-over 后仍能监听后续写入。
- `auth-dir` 改动先校验并注册新目录，成功后再退役旧 watch、发布配置和凭据快照。共享配置目录 watch 不可一起删除；旧目录排队中的事件不得污染新快照。
- hash 来自 `LoadConfig` 返回的配置快照，不从 callback 完成后的文件内容推断。解析器可能把明文管理密码改写成 bcrypt；此改写不应造成重复应用循环，也不能吞掉 callback 期间发生的下一次编辑。
- 后续 reload 等待前次 callback 完成，不能交错提交；慢 callback 会延迟更新，不宣称 callback 已异步化。
- 对象存储仅在 listing 所有页面、下载和遍历均成功后删除未出现的本地 JSON 镜像；失败或取消时不清理 stale 文件。已成功下载的文件可以保留，不承诺整批原子下载。
- 不使用 `RemoveAll`。仅删除确认 stale 的镜像文件，保留目录及非凭据文件；本次对账产生的删除不能经 watcher 再删除远端对象。文件恢复后取消旧抑制状态，用户显式删除仍正常执行。

## 4. 错误矩阵

| 条件 | 预期 |
| --- | --- |
| 配置连续原子替换后再次普通写入 | 每次都能应用，不丢 watch |
| 新 auth-dir 不存在或不是目录 | 保留旧配置、hash、watch、凭据快照 |
| callback 应用 A 时文件变成 B | A 的 hash 不代表 B；下一次 reload 应用 B |
| S3 listing 后续页、下载或遍历失败 | 不清理现有 stale 文件 |
| 全部同步成功且远端已删某凭据 | 删除对应本地镜像，不恢复旧凭据，不反向 DELETE 远端 |

## 5. 正常、基础与错误案例

- 正常：配置从目录 A 切到 B，A 凭据删除、B 凭据加入，新建 B 文件继续产生增量更新。
- 基础：仅修改 YAML 注释，运行时配置不必重复应用，但镜像仍可持久化注释。
- 错误：同步启动先清空 auth 目录，再下载；部分失败会丢凭据，watcher 还可能反向删除远端对象。

## 6. 必须测试

用隔离文件目录和合成 S3 transport 覆盖连续 rename-over、目录切换/共享 watch/失败回滚、旧事件隔离、callback 中修改下一版、bcrypt 改写；S3 多页 listing、失败/取消保护、删除回传抑制、恢复后重新删除。race 检查快照和目录字段，顺序断言使用 channel，不用墙钟 Sleep。

## 7. 错误与正确写法

错误：应用配置 A 后重新读文件 B，并把 B 的 hash 记录为当前状态。

正确：hash 绑定实际解析并应用的 A；B 的独立事件仍触发下一次加载。
