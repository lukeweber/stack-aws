pipeline {
    agent { label 'upbound-gce' }

    parameters {
        booleanParam(defaultValue: true, description: 'Execute pipeline?', name: 'shouldBuild')
    }

    options {
        disableConcurrentBuilds()
        timestamps()
    }

    environment {
        RUNNING_IN_CI = 'true'
        REPOSITORY_NAME = "${env.GIT_URL.tokenize('/')[3].split('\\.')[0]}"
        REPOSITORY_OWNER = "${env.GIT_URL.tokenize('/')[2]}"
        DOCKER = credentials('dockerhub-upboundci')
        AWS = credentials('aws-upbound-bot')
        GITHUB_UPBOUND_BOT = credentials('github-upbound-jenkins')
    }

    stages {

        stage('Prepare') {
            steps {
                script {
                    if (env.CHANGE_ID != null) {
                        def json = sh (script: "curl -s https://api.github.com/repos/crossplaneio/stack-aws/pulls/${env.CHANGE_ID}", returnStdout: true).trim()
                        def body = evaluateJson(json,'${json.body}')
                        if (body.contains("[skip ci]")) {
                            echo ("'[skip ci]' spotted in PR body text.")
                            env.shouldBuild = "false"
                        }
                    }
                }
                sh 'git config --global user.name "upbound-bot"'
                sh 'echo "machine github.com login upbound-bot password $GITHUB_UPBOUND_BOT" > ~/.netrc'
            }
        }

        stage('Build'){
            when {
                expression {
                    return env.shouldBuild != "false"
                }
            }
            steps {
                sh './build/run make vendor.check'
                sh './build/run make -j\$(nproc) build.all'
            }
            post {
                always {
                    archiveArtifacts "_output/lint/**/*"
                }
            }
        }

        stage('Unit Tests') {
            when {
                expression {
                    return env.shouldBuild != "false"
                }
            }
            steps {
                sh './build/run make -j\$(nproc) test'
                sh './build/run make -j\$(nproc) cobertura'
            }
            post {
                always {
                    archiveArtifacts "_output/tests/**/*"
                    junit "_output/tests/**/unit-tests.xml"
                    cobertura coberturaReportFile: '_output/tests/**/cobertura-coverage.xml',
                            classCoverageTargets: '50, 0, 0',
                            conditionalCoverageTargets: '70, 0, 0',
                            lineCoverageTargets: '40, 0, 0',
                            methodCoverageTargets: '30, 0, 0',
                            packageCoverageTargets: '80, 0, 0',
                            autoUpdateHealth: false,
                            autoUpdateStability: false,
                            enableNewApi: false,
                            failUnhealthy: false,
                            failUnstable: false,
                            maxNumberOfBuilds: 0,
                            onlyStable: false,
                            sourceEncoding: 'ASCII',
                            zoomCoverageChart: false
                }
            }
        }

        stage('Record Coverage') {
            when {
                allOf {
                    branch 'master';
                    expression {
                        return env.shouldBuild != "false"
                    }
                }
            }
            steps {
                script {
                    currentBuild.result = 'SUCCESS'
                 }
                step([$class: 'MasterCoverageAction', scmVars: [GIT_URL: env.GIT_URL]])
            }
        }

        stage('PR Coverage to Github') {
            when {
                allOf {
                    not { branch 'master' };
                    expression { return env.CHANGE_ID != null };
                    expression { return env.shouldBuild != "false"}
                }
            }
            steps {
                script {
                    currentBuild.result = 'SUCCESS'
                }
                step([$class: 'CompareCoverageAction', publishResultAs: 'comment', scmVars: [GIT_URL: env.GIT_URL]])
            }
        }

        stage('Publish') {
            when {
                expression {
                    return env.shouldBuild != "false"
                }
            }
            steps {
                sh 'docker login -u="${DOCKER_USR}" -p="${DOCKER_PSW}"'
                sh "./build/run make -j\$(nproc) publish BRANCH_NAME=${BRANCH_NAME} AWS_ACCESS_KEY_ID=${AWS_USR} AWS_SECRET_ACCESS_KEY=${AWS_PSW} GIT_API_TOKEN=${GITHUB_UPBOUND_BOT}"
                script {
                    if (BRANCH_NAME == 'master') {
                        lock('promote-job') {
                            sh "./build/run make -j\$(nproc) promote BRANCH_NAME=master CHANNEL=master AWS_ACCESS_KEY_ID=${AWS_USR} AWS_SECRET_ACCESS_KEY=${AWS_PSW}"
                        }
                    }
                }
            }
        }
    }

    post {
        always {
            script {
                sh 'make -j\$(nproc) clean'
                sh 'make -j\$(nproc) prune PRUNE_HOURS=48 PRUNE_KEEP=48'
                sh 'docker images'
                deleteDir()
            }
        }
    }
}

@NonCPS
def evaluateJson(String json, String gpath){
    // parse json
    def ojson = new groovy.json.JsonSlurper().parseText(json)
    // evaluate gpath as a gstring template where $json is a parsed json parameter
    return new groovy.text.GStringTemplateEngine().createTemplate(gpath).make(json:ojson).toString()
}
