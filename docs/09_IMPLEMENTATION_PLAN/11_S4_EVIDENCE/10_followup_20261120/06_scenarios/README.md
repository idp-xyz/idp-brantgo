# F2 与 R14 改过的场景（2026-11-20）

这些文件是 E3 在本工作树（分支 s4/e3-archive，基线 9d2d045）上重跑产生的，不是 2026-11-13 或 2026-11-17 那两份档案记录的改写。

被盖过的只有这些场景：

- `08_s5r_regression/s5r_records/S5R-D05.json` 里「确认优先仍批准 50」这一句。新文件是本目录的 `S5R-D05.json`（测试 `TestS5R12D05DispositionAndTheShipmentOverHTTP`，环境变量 `BRANT_S5_SCENARIOS_REPORT` 指向本目录）。那份旧文件的其他图不因本目录作废。
- S5 证据里 CN-AT18 关于「AI 令牌经 HTTP 导入并准备」的那次观察。新文件是本目录的 `cn-at18.json`（测试 `TestCNATSuite18AIReadsInvoicesAndDeclarations`，环境变量 `BRANT_CNAT_EVIDENCE` 指向本目录）。2026-11-13 的原件不改。

同一次还跑了 `TestDeclarationProfileOverHTTP` 与 `TestS5RD05ConfirmationFirstMakesTheApprovalWait`。它们不写场景 JSON。结果行在本目录的 `extra_tests.txt`，只留测试名与通过或失败，不留数据库连接串。

没有执行：真实模型套件、`make eer-live`、`make race`、L1、本档案提交的远程 CI。
