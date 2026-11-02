-- Abyssal-City 数据库初始化
-- 挂载到 MySQL 容器的 /docker-entrypoint-initdb.d 后，首次启动自动执行

-- 账号表：账号 + 密码哈希（绝不存明文）
CREATE TABLE IF NOT EXISTS account (
    id            BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY COMMENT '账号ID，即玩家uid',
    account       VARCHAR(64)  NOT NULL UNIQUE COMMENT '登录账号',
    password_hash VARCHAR(100) NOT NULL COMMENT 'bcrypt 哈希',
    created_at    TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- 玩家角色表：基础信息，uid 关联 account.id
CREATE TABLE IF NOT EXISTS player (
    uid        BIGINT UNSIGNED PRIMARY KEY COMMENT '玩家ID = account.id',
    name       VARCHAR(64) NOT NULL DEFAULT '' COMMENT '角色名',
    level      INT         NOT NULL DEFAULT 1 COMMENT '等级',
    stamina    INT         NOT NULL DEFAULT 120 COMMENT '体力',
    exp        INT         NOT NULL DEFAULT 0 COMMENT '经验（战斗结算累积）',
    gold       INT         NOT NULL DEFAULT 0 COMMENT '金币（战斗结算累积）',
    created_at TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (uid) REFERENCES account (id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;

-- 战斗记录表：每场结算落一条，uid 关联 player.uid
CREATE TABLE IF NOT EXISTS battle_record (
    id           BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY COMMENT '记录ID',
    uid          BIGINT UNSIGNED NOT NULL COMMENT '玩家ID',
    result       INT         NOT NULL COMMENT '0 胜 / 1 败',
    score        INT         NOT NULL DEFAULT 0 COMMENT '结算分数',
    duration_sec INT         NOT NULL DEFAULT 0 COMMENT '战斗时长（秒）',
    rewards      VARCHAR(255) NOT NULL DEFAULT '' COMMENT '奖励描述，如 exp:100,gold:500',
    created_at   TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (uid) REFERENCES player (uid)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4;
