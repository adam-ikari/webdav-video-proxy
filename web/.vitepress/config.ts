import { defineConfig } from 'vitepress'

// 仓库名 webdav-video-proxy，GitHub Pages 项目站点路径为 /webdav-video-proxy/，
// 故 base 必须设为此值，否则资源 404。
export default defineConfig({
  lang: 'zh-CN',
  title: 'WebDAV 视频网盘代理',
  description: 'Docker 部署的 WebDAV 视频代理，解决多源网盘播放卡顿、起播慢、拖动卡',
  base: '/webdav-video-proxy/',
  lastUpdated: true,
  cleanUrls: true,

  themeConfig: {
    nav: [
      { text: '首页', link: '/' },
      {
        text: '指南',
        link: '/guide/getting-started',
        activeMatch: '/guide/'
      },
      { text: 'GitHub', link: 'https://github.com/adam-ikari/webdav-video-proxy' }
    ],

    sidebar: {
      '/guide/': [
        {
          text: '开始',
          items: [
            { text: '快速开始', link: '/guide/getting-started' },
            { text: '架构总览', link: '/guide/architecture' }
          ]
        },
        {
          text: '功能',
          items: [
            { text: '核心功能', link: '/guide/features' },
            { text: '透明降级', link: '/guide/degradation' }
          ]
        },
        {
          text: '运维',
          items: [
            { text: '部署与配置', link: '/guide/deployment' }
          ]
        }
      ]
    },

    socialLinks: [
      { icon: 'github', link: 'https://github.com/adam-ikari/webdav-video-proxy' }
    ],

    outline: {
      label: '本页导航',
      level: [2, 3]
    },

    docFooter: {
      prev: '上一页',
      next: '下一页'
    },

    lastUpdatedText: '最后更新',

    search: {
      provider: 'local',
      options: {
        translations: {
          button: {
            buttonText: '搜索文档',
            buttonAriaLabel: '搜索文档'
          },
          modal: {
            displayDetails: '显示详情',
            resetButtonTitle: '清除查询',
            backButtonTitle: '关闭',
            noResultsText: '没有结果',
            footer: {
              selectText: '选择',
              navigateText: '切换',
              closeText: '关闭'
            }
          }
        }
      }
    }
  }
})
