# Release v1.1.26-test

- 基于 `dev_open` 集成官方审核结果增强：改进 LLM 工具调用错误处理和上下文恢复。
- 改进工具调用 Token 统计，降低长上下文审核结果被截断的风险。
- 支持更长的 OpenCode 背景上下文。
- 文件重命名后保留历史审核发现。
- 修复非 ASCII 文件路径在搜索结果中的显示和定位。

安装：

```sh
npm install -g open-code-review-enhanced@1.1.26-test --allow-scripts=open-code-review-enhanced
```
