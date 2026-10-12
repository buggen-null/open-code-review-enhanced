# 1.1.39-test

- Prevent review tasks from attaching to and reporting an older session as their own.
- Let large individual file reviews run without the default per-file timeout; the viewer task deadline still limits total runtime.
- Allow administrators to configure a per-file review timeout with `OCR_VIEWER_REVIEW_TASK_TIMEOUT`.
