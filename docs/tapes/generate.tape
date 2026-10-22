Output docs/images/generate.gif
Source docs/tapes/common.tape
Type "gitopsify generate --from-file dump.yaml --tool argocd --env uat \"
Enter
Type "  --repo-url https://github.com/acme/gitops.git --secrets local"
Sleep 400ms
Enter
Sleep 4s
Type "ls gitops gitops/argocd/uat/apps"
Enter
Sleep 2s
Type "head -20 gitops/argocd/uat/apps/shop.yaml"
Enter
Sleep 5s
