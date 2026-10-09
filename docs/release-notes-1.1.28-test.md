# Release v1.1.28-test

- 修复反向代理下取消审核任务后的跳转路径。
- 修复取消任务后后台审核继续更新状态的问题。
- 默认并发审核任务数调整为 5，第 6 个任务开始排队。
- 优化审核任务表格列宽和长文本换行。
- 修复仓库删除按钮事件未生效及审核任务删除入口。

安装：

```sh
npm install -g open-code-review-enhanced@1.1.28-test --allow-scripts=open-code-review-enhanced
```
