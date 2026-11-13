# S5-11 容量基准与恢复演练的原始输出（单元 C7，2026-11-12）

原样复制自单元 C7 的证据目录（`.claude/handover/scratchpad/c7/evidence/`），未作修改；路径中的 `/home/tops/...` 是 C7 工作树内的输出位置。汇总与结论见 S5 执行报告第 06 节。

环境：一台 WSL2 开发主机（内核 6.18.58.1-microsoft-standard-WSL2，20 个逻辑处理器，64 GiB 内存），Go 1.26.8 linux/amd64；PostgreSQL 16.12（CI 所用 digest 的 Alpine 镜像，Docker，未调优：shared_buffers 160 MB、max_connections 100、synchronous_commit on、fsync on、wal_level replica、archive_mode on、archive_timeout 60 s）；源库容器 brant-pg16-c7、恢复目标容器 brant-pg16-c7r；负载工具、brant-api、brant-worker 与数据库在同一主机，主机上同时有其他单元的测试与其他项目。负载工具在每个阶段前后记录 uptime、处理器数与内存。

| 文件 | 内容 |
|---|---|
| load-tables.md | 两轮阶梯负载与第三轮实验的完整汇总表（含 API 核数、循环 P95、死锁与回滚、中继延迟最大值） |
| run1/、run2/ | 阶梯 1、2、4、8、16、32、64 个命令用户（加同数读取用户）的报告 report.json 与 summary.md：每阶 10 s 预热、50 s 测量，逐路由与逐命令的分位数 |
| run3/ | 实验：两个外部分发进程，阶梯 4、8、16 |
| drain1-worker.log | 第一轮之后中继清空积压的记录 |
| drill1/ | 第 1 轮演练（make drill：逻辑、PITR、升级）：drill.log、facts.tsv、恢复前后的表摘要与迁移账本、已确认写入的核对、升级迁移日志与冒烟结果 |
| drill2/ | 第 2 轮演练（逻辑与 PITR，较大的库）：drill.log、facts.tsv 与已确认写入的核对 |

未执行：生产或准生产主机；真实负载模型与多租户；负载期间的故障注入（除 PITR 演练中的数据库 SIGKILL）；异地或对象存储的归档与保留；复制与故障转移；构建、配置与凭据的恢复；浸泡测试；调优实验；成本。归档卷与基础备份与源库在同一磁盘，归档命令为不带 fsync 的 cp。G4 仍为 NO-GO。
