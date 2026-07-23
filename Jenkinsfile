// Pandora 后端流水线:git 提交 → 全模块 build/test → 业务镜像离线包发布到制品目录。
// 与客户端 Tool/Build/Jenkinsfile(UE 打包 → PublishPackages)共用同一个制品根,
// 布局与规则见 docs/design/release-pipeline.md。
pipeline {
    // 构建机需具备:Go 1.26.5(宿主交叉编译)、Docker Desktop、pwsh、git。
    agent { label 'windows' }

    options {
        timestamps()
        disableConcurrentBuilds()
        buildDiscarder(logRotator(numToKeepStr: '30'))
    }

    triggers {
        // 哈希错峰轮询,与客户端流水线一致。
        pollSCM('H/5 * * * *')
    }

    parameters {
        booleanParam(
            // 测试全绿后是否构建并发布业务镜像离线包(publish_offline_images.ps1)。
            name: 'PUBLISH_IMAGES',
            defaultValue: true,
            description: 'Build the 21 business images and publish the offline tar to the artifact directory.'
        )
        string(
            name: 'ARTIFACT_ROOT_OVERRIDE',
            defaultValue: '',
            trim: true,
            description: 'Optional artifact root override. Empty preserves the agent PANDORA_ARTIFACT_ROOT and then falls back to F:\\work\\artifacts.'
        )
    }

    stages {
        stage('Checkout') {
            steps {
                checkout scm
            }
        }

        stage('Build & Test') {
            steps {
                bat 'pwsh -NoProfile -ExecutionPolicy Bypass -File tools\\scripts\\ci_backend.ps1'
            }
        }

        stage('Publish Offline Images') {
            when {
                expression { return params.PUBLISH_IMAGES }
            }
            steps {
                script {
                    // 发布脚本自带:clean 工作区强制、git sha 版本戳、不可变 + 原子发布、
                    // 同 sha 已发布则 -SkipIfExists 幂等跳过。
                    def publishEnv = []
                    def artifactRootOverride = params.ARTIFACT_ROOT_OVERRIDE?.trim()
                    if (artifactRootOverride) {
                        publishEnv << "PANDORA_ARTIFACT_ROOT=${artifactRootOverride}"
                    }
                    def publishCmd = 'pwsh -NoProfile -ExecutionPolicy Bypass -File tools\\scripts\\publish_offline_images.ps1 -SkipIfExists'
                    if (publishEnv) {
                        withEnv(publishEnv) {
                            bat publishCmd
                        }
                    } else {
                        bat publishCmd
                    }
                }
            }
        }
    }
}
